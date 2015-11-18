package dttserver

import (
	"DTT/libstore"
	"DTT/paxos"
	"DTT/rpc/dttrpc"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DEC_BASE     = 10
	DTT_RPC      = "DttServer"
	MAX_DTTS     = 100
	TIMEOUT_SECS = 5

	E_POSTFIX     = ":E" // bool indicator for user exist E_<userid>
	SUB_POSTFIX   = ":SUB"
	SJOB_PREFIX   = "SJOB:" // superjob prefix
	LOC_PREFIX    = "IMGLOC:"
	TRANS_PREFIX  = "TRANS:"
	IMGJOB_PREFIX = "IMG" // prefix for image jobs
	SJOB_LIST_KEY = "SUPER_JOB_LIST"
	SJOB_MAXID    = "SUPER_JOB_MAXID"

	SJOB_LOG_PREFIX  = "SJOBLOG_"
	TRANS_LOG_PREFIX = "TRANS_"
	// Format of  dtt ids : "userid;timestamp;hashofcontents"

	REF_PREFIX  = "ref/"
	TEST_PREFIX = "test/"
)

var TS DttServer

type dttServer struct {
	// TODO: implement this!
	Lstore libstore.Libstore
	Pnode  paxos.PaxosNode

	// Tx Mutex
	TxMutex *sync.Mutex

	// Log constructs MUTEX
	LogMutex *sync.Mutex

	Sjob_log      map[int]*LogEntry
	Sjob_log_chan map[int]chan struct{}

	Sjob_logID  int
	Sjob_currID int
}

type TranscribeSubmitInfo struct {
	Id            string
	Transcription string
}

func NewDttServer(masterServerHostPort, myHostPort string, allHostPorts []string, numSrvs, srvID int, replace bool) (DttServer, error) {

	// TODO: filter out the server's own hostport string
	ts := new(dttServer)
	ts.Sjob_log = make(map[int]*LogEntry)
	ts.Sjob_log_chan = make(map[int]chan struct{})
	ts.Sjob_logID = 0
	ts.Sjob_currID = 0
	ts.LogMutex = new(sync.Mutex)
	ts.TxMutex = new(sync.Mutex)
	// Create random seed for randomness
	rand.Seed(time.Now().UTC().UnixNano())

	//fmt.Printf("Listeneing")
	// Create the server socket that will listen for incoming RPCs.
	listener, err := net.Listen("tcp", myHostPort)
	if err != nil {
		return nil, err
	}

	http.HandleFunc("/", handlerGeneric)

	TS = ts

	// Handle RPC calls for Libstore
	rpc.HandleHTTP()

	// Setup the HTTP handler that will
	// serve requests in a background goroutine.
	go http.Serve(listener, nil)

	return ts, nil
}

// TODO: when requester client connects, create new superJob ID
func (ts *dttServer) NewSuperJob(jobsList []string) error {
	var err error

	sjob := new(LogEntry)
	sjob.Type = AddSuperJob
	sjob.EncapData, err = json.Marshal(SuperJobVal{jobsList})

	if err != nil {
		return err
	}
	err = ts.NewLogEntry(sjob)
	return err
}

func (ts *dttServer) SaveSuperJob(sjid int, sjob *SuperJobVal) error {

	jobsList := sjob.JobsList

	var err error

	jidList := make([]int, len(jobsList), len(jobsList))
	// generate job IDs
	// any cleaner way to do this in go?
	for i := 0; i < len(jobsList); i++ {
		jidList[i] = i
	}

	// add individual jobs to location data store
	// jobs will be added to transcription data store when transcription is received
	for _, j := range jidList {
		err = ts.Lstore.Put(LOC_PREFIX+makeImgJobKey(sjid, j), jobsList[j])
		if err != nil {
			return err
		}
		// add superjob to data store
		err = ts.Lstore.AppendToList(SJOB_PREFIX+strconv.Itoa(sjid), strconv.Itoa(j))
	}

	if err != nil {
		return err
	}

	err = ts.Lstore.AppendToList(SJOB_LIST_KEY, strconv.Itoa(sjid))

	if err != nil {
		return err
	}

	return nil

}

func (ts *dttServer) GetAllSjobs() ([]int, error) {

	list, err := ts.Lstore.GetList(SJOB_LIST_KEY)

	if err != nil {
		return nil, err
	}

	sjidlist := make([]int, len(list), len(list))

	for i, v := range list {
		num, _ := strconv.Atoi(v)
		sjidlist[i] = num
	}

	return sjidlist, nil
}

func (ts *dttServer) GetJobList(sjid int) ([]int, error) {

	list, err := ts.Lstore.GetList(SJOB_PREFIX + strconv.Itoa(sjid))

	if err != nil {
		return nil, err
	}

	jidlist := make([]int, len(list), len(list))

	for i, v := range list {
		num, _ := strconv.Atoi(v)
		jidlist[i] = num
	}

	return jidlist, nil
}

func (ts *dttServer) GetSjobJobs(sjid int) ([]*dttrpc.Job, error) {

	jidList, err := ts.GetJobList(sjid)

	if err != nil {
		return nil, err
	}

	transcripts := make([]*dttrpc.Job, len(jidList), len(jidList))

	for i, v := range jidList {
		loc, imgerr := ts.Lstore.Get(LOC_PREFIX + makeImgJobKey(sjid, v))
		if imgerr != nil {
			return nil, imgerr
		}

		trans, transerr := ts.Lstore.Get(TRANS_PREFIX + makeImgJobKey(sjid, v))
		if transerr != nil && transerr.Error() != "KeyNotFound" {
			return nil, transerr
		}

		job := new(dttrpc.Job)
		job.Sid = sjid
		job.Jid = v
		job.Loc = loc
		job.Transcript = trans

		transcripts[i] = job
	}

	return transcripts, nil
}

func (ts *dttServer) GetImage() (*dttrpc.Job, error) {

	// get random image job from server
	sjobs, err := ts.GetAllSjobs()
	if err != nil {
		return nil, err
	}

	sid := sjobs[rand.Intn(len(sjobs))]

	jobs, jerr := ts.GetSjobJobs(sid)
	if jerr != nil {
		return nil, jerr
	}
	jid := jobs[rand.Intn(len(jobs))].Jid

	imgLoc, err := ts.Lstore.Get(LOC_PREFIX + makeImgJobKey(sid, jid))

	if err != nil {
		return nil, err
	}

	job := new(dttrpc.Job)
	job.Sid = sid
	job.Jid = jid
	job.Loc = imgLoc

	return job, nil

}

func (ts *dttServer) SetTranscription(job *dttrpc.Job) error {

	var err error

	// TODO: remove this when structs get cleaned up
	val := new(TranscriptVal)
	val.Sid = job.Sid
	val.Jid = job.Jid
	val.Loc = job.Loc
	val.Transcript = job.Transcript

	transcript := new(LogEntry)
	transcript.Type = SetTranscription
	transcript.EncapData, err = json.Marshal(val)

	key := makeImgJobKey(job.Sid, job.Jid)

	// check whether a transcription exists
	_, founderr := ts.Lstore.Get(LOC_PREFIX + key)

	if founderr != nil {
		return errors.New("ERROR: Job does not exist")
	}

	curTrans, err := ts.Lstore.Get(TRANS_PREFIX + key)

	if err == nil {
		better := chooseTrans(curTrans, job.Transcript)
		if better != curTrans {
			return ts.NewLogEntry(transcript)
		}
	} else {
		return ts.NewLogEntry(transcript)
	}

	return nil
}

func (ts *dttServer) SaveTranscription(job *TranscriptVal) error {

	key := makeImgJobKey(job.Sid, job.Jid)

	return ts.Lstore.Put(TRANS_PREFIX+key, job.Transcript)
}

func (ts *dttServer) ProcessJobs() {
	for {
		// process
		ts.LogMutex.Lock()
		loge, ok := ts.Sjob_log[ts.Sjob_currID]
		if !ok {
			ts.LogMutex.Unlock()
			log.Printf("BREAK\n")
			return
			break
		}
		logId := ts.Sjob_currID
		//fmt.Printf("Processing job : %v",logId)
		ts.LogMutex.Unlock()

		// Case
		switch loge.Type {

		case AddSuperJob:
			// Unmarshal into superjobVal
			var sjob SuperJobVal
			err := json.Unmarshal(loge.EncapData, &sjob)
			if err != nil {
				log.Printf("ERR:%v\n", err)
				return
			}

			log.Printf("SAVED SUPERJOb\n")
			ts.SaveSuperJob(logId, &sjob)

		case SetTranscription:
			// Unmarshal into superjobVal
			var tval TranscriptVal
			err := json.Unmarshal(loge.EncapData, &tval)
			if err != nil {
				log.Printf("ERR:%v\n", err)
				return
			}
			// Call set transcription

			log.Printf("SET TRANS\n")
			ts.SaveTranscription(&tval)

		case AddCodeJob:
			// Unmarshal into CodeJob
			var cjob CodeJob
			err := json.Unmarshal(loge.EncapData, &cjob)
			if err != nil {
				log.Printf("ERR:%v\n", err)
				return
			}
			// Call  add code job

			log.Printf("Add Super Job\n")
			ts.SaveCodeJob(logId, &cjob)

		case SetOptimize:
			// Unmarshal into CodeJob
			var cjob CodeJob
			err := json.Unmarshal(loge.EncapData, &cjob)
			if err != nil {
				log.Printf("ERR:%v\n", err)
				return
			}
			// Call  add code job

			log.Printf("Add Super Job\n")
			ts.SaveOptimize(&cjob)

		case Noop:
		default:
			// Do none
		}

		ts.LogMutex.Lock()
		// Increment id to wait on
		ts.Sjob_currID += 1
		ts.LogMutex.Unlock()
	}
}

func (ts *dttServer) SendNoop(logId int) {
	key := SJOB_LOG_PREFIX + strconv.Itoa(logId)
	value, err := json.Marshal(&LogEntry{Noop, nil})
	if err != nil {
		//fmt.Printf("Marshal Error:%v\n",err)
		return
	}
	//fmt.Printf("SENDIGN NOOOP\n\n\n")
	ts.Pnode.TryVal(key, value)
}

func (ts *dttServer) CatchUp() {
	//fmt.Printf("Call catchup!")
	for {
		timer := time.After(time.Duration(TIMEOUT_SECS) * time.Second)

		<-timer

		ts.LogMutex.Lock()
		reqId := ts.Sjob_currID
		ts.LogMutex.Unlock()
		for {

			ts.LogMutex.Lock()

			logId := ts.Sjob_logID
			if reqId < ts.Sjob_currID {
				reqId = ts.Sjob_currID
			}

			// Get chan to wait on
			keychan, ok := ts.Sjob_log_chan[reqId]

			if !ok {
				ts.Sjob_log_chan[reqId] = make(chan struct{})
				keychan, _ = ts.Sjob_log_chan[reqId]
			}

			_, committed := ts.Sjob_log[reqId]

			// Unlock mutex lock
			ts.LogMutex.Unlock()

			if reqId >= logId {
				break

			} else {
				// Send Noop and wait
				if !committed {
					ts.SendNoop(reqId)

					//fmt.Printf("Sending No-op: %v\n",reqId)
				}

				timer2 := time.After(time.Duration(TIMEOUT_SECS) * time.Second)
				select {
				// We close when it commits, so all waiting will stop
				case <-keychan:
					// Incremenet reqId by 1 if got this one
					reqId += 1

				// time out if no value received for TIMEOUT_SECS
				case <-timer2:
				}

			}

		}
	}

}
func (ts *dttServer) ValueReceiver() {
	// keep
	for {
		// Get the next key/value pair commited by paxos
		key, val, err := ts.Pnode.RecvVal()

		log.Printf("RECV VALUE: (%v,%v)\n", key, val)
		if err != nil {
			continue
		}

		// Mutex lock somewhere here! TODO

		// If sjob log
		// Handle SJOB Log prefix

		logId, err := strconv.Atoi(key[len(SJOB_LOG_PREFIX):])

		//fmt.Printf("recv %v\n",logId)

		if err != nil {
			//fmt.Printf("ERR:%v\n",err)
			log.Fatalf("BOOM1")
		}

		var loge LogEntry
		err = json.Unmarshal(val, &loge)
		if err != nil {
			fmt.Printf("ERR:%v\n", err)
			log.Fatalf("\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\nFATAL VALUE: %v\n\n\n\n", val)

		}

		ts.LogMutex.Lock()
		ts.Sjob_log[logId] = &loge
		keychan, ok := ts.Sjob_log_chan[logId]

		if !ok {
			ts.Sjob_log_chan[logId] = make(chan struct{})
		}

		// Update ID of next log Id
		if logId+1 > ts.Sjob_logID {
			ts.Sjob_logID = logId + 1
		}

		keychan, ok = ts.Sjob_log_chan[logId]
		close(keychan)

		currId := ts.Sjob_currID
		ts.LogMutex.Unlock()

		log.Printf("JOBS!!! (%v,%v)\n\n", currId, logId)
		// If the gotten log ID is the next to process, process it
		if logId == currId {
			// Process it
			log.Printf("Entering Process JOBS!!! (%v,%v)\n\n", currId, logId)
			ts.ProcessJobs()
			log.Printf("Exit from Process JOBS!!!\n\n")
		} /*else {
		    // Send Noop for currId to current
		    for i:= currId ; i < logId; i++ {
		        ts.SendNoop(i)
		    }
		}*/

	}
}

func (ts *dttServer) NewCodeJob(job, title string) error {
	var err error

	codeJob := new(CodeJob)

	codeJob.Title = title
	codeJob.OldCode = job

	sjob := new(LogEntry)
	sjob.Type = AddCodeJob
	sjob.EncapData, err = json.Marshal(codeJob)

	if err != nil {
		return err
	}
	err = ts.NewLogEntry(sjob)
	return err
}

func (ts *dttServer) SaveCodeJob(sjid int, sjob *CodeJob) error {

	var err error

	key := strconv.Itoa(sjid)

	// store ID to code
	err = ts.Lstore.Put(OLDCODE_PREFIX+key, sjob.OldCode)
	if err != nil {
		return err
	}

	// store ID to title
	err = ts.Lstore.Put(TITLE_PREFIX+key, sjob.Title)
	if err != nil {
		return err
	}

	err = ts.Lstore.Put(NEWCODE_PREFIX+key, "")
	if err != nil {
		return err
	}

	err = ts.Lstore.Put(NAME_PREFIX+key, "")
	if err != nil {
		return err
	}

	// store ID: time, where time is the time of the refsol (MaxFloat if refsol times out)

	time := math.MaxFloat64

	timeStr := fmt.Sprintf("%.2f", time)

	err = ts.Lstore.Put(TIME_PREFIX+key, timeStr)

	if err != nil {
		return err
	}

	refpath := REF_PREFIX + strconv.Itoa(sjid) + ".py"
	// Put in code into test path
	err = ioutil.WriteFile(refpath, []byte(sjob.OldCode), os.ModePerm)
	if err != nil {
		return err
	}

	// add superjob to data store
	err = ts.Lstore.AppendToList(CODE_LIST_KEY, key)

	if err != nil {
		return err
	}

	return nil

}

func (ts *dttServer) GetAllCodeJobs() ([]*CodeJob, error) {

	allCode, err := ts.Lstore.GetList(CODE_LIST_KEY)

	if err != nil {
		return nil, err
	}

	codes := make([]*CodeJob, len(allCode), len(allCode))

	for i, v := range allCode {

		// retrieve each field of the CodeJob
		title, titleerr := ts.Lstore.Get(TITLE_PREFIX + v)
		if titleerr != nil {
			return nil, titleerr
		}

		oldCode, oldcodeerr := ts.Lstore.Get(OLDCODE_PREFIX + v)

		if oldcodeerr != nil {
			return nil, oldcodeerr
		}

		newCode, transerr := ts.Lstore.Get(NEWCODE_PREFIX + v)

		if transerr != nil && transerr.Error() != "KeyNotFound" {
			return nil, transerr
		}

		name, nameerr := ts.Lstore.Get(NAME_PREFIX + v)

		if nameerr != nil && nameerr.Error() != "KeyNotFound" {
			return nil, nameerr
		}

		time, timeerr := ts.Lstore.Get(TIME_PREFIX + v)

		if timeerr != nil {
			return nil, timeerr
		}

		timeFloat, _ := strconv.ParseFloat(time, 64)

		// construct CodeJob
		job := new(CodeJob)
		job.Jid, _ = strconv.Atoi(v)
		job.Title = title
		job.OldCode = oldCode
		job.NewCode = newCode
		job.Name = name
		job.Time = timeFloat

		codes[i] = job
	}

	return codes, nil
}

// Checks for unhandled errors
func CheckGenericError(err error) bool {
	if err == nil {
		return false
	}

	errmsg := err.Error()

	return (errmsg != "OK" &&
		errmsg != "KeyNotFound" &&
		errmsg != "ItemNotFound" &&
		errmsg != "ItemExists" &&
		errmsg != "WrongServer" &&
		errmsg != "NotReady")

}

func makeImgJobKey(superjob, imgjob int) string {
	return IMGJOB_PREFIX + strconv.Itoa(superjob) + ":" + strconv.Itoa(imgjob)
}

func chooseTrans(curTrans, newTrans string) string {
	// TODO: make actual heuristic
	return newTrans
}

/***** HTTP HANDLERS *****/

func handler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Hi there, I love %s!", r.URL.Path[1:])
}

func handlerTesterTranscribe(w http.ResponseWriter, r *http.Request) {
	// Input format in Sid:Jid:trans
	str := r.URL.Path[len("/tester/transcribe/"):]
	params := strings.Split(str, ":")
	sid, _ := strconv.Atoi(params[0])
	jid, _ := strconv.Atoi(params[1])
	trans := params[2]

	setjob := dttrpc.Job{sid, jid, "", trans}
	TS.SetTranscription(&setjob)
	fmt.Fprintf(w, "sucess")
}

func handlerTesterAddSjob(w http.ResponseWriter, r *http.Request) {
	jobs_str := r.URL.Path[len("/tester/addsjob/"):]
	jobs := strings.Split(jobs_str, ";")

	TS.NewSuperJob(jobs)
	fmt.Fprintf(w, "sucess")
}

func handlerTesterGet(w http.ResponseWriter, r *http.Request) {
	sjobs, err := TS.GetAllSjobs()
	if err != nil {
		return
	}

	for i1, v := range sjobs {
		jobs, err := TS.GetSjobJobs(v)
		if err != nil {
			return
		}

		for i2, j := range jobs {
			fmt.Fprintf(w, "%v;%v;%v;%v", j.Sid, j.Jid, j.Loc, j.Transcript)
			if i2 < len(jobs)-1 {
				fmt.Fprintf(w, "#")
			}
		}
		if i1 < len(sjobs)-1 {
			fmt.Fprintf(w, "###")
		}
	}
}

type TranscribeInfo struct {
	Id     string
	ImgURL string
}

func handlerTranscribe(w http.ResponseWriter, r *http.Request) {

	// Call server function

	dttjob, err := TS.GetImage()

	if err != nil {
		fmt.Fprintf(w, "Error: %v\n", err)
		return
	}

	id := strconv.Itoa(dttjob.Sid) + ":" + strconv.Itoa(dttjob.Jid)
	img_url := dttjob.Loc

	p := &TranscribeInfo{Id: id, ImgURL: img_url}
	t, _ := template.ParseFiles("transcribe.html")
	t.Execute(w, p)
}

func handlerTranscribeSubmit(w http.ResponseWriter, r *http.Request) {
	// Get random Img from DB
	id := r.URL.Path[len("/transcribe_submit/"):]
	transcription := r.FormValue("transcription")
	p := &TranscribeSubmitInfo{Id: id, Transcription: transcription}

	// split
	idarr := strings.Split(id, ":")
	if len(idarr) != 2 {
		fmt.Fprintf(w, "ID Array not eq to 2\n")
		return
	}

	sid, serr := strconv.Atoi(idarr[0])
	jid, jerr := strconv.Atoi(idarr[1])
	if serr != nil || jerr != nil {
		fmt.Fprintf(w, "Error converting IDs\n")
		return
	}

	// TODO CHANGE
	setjob := dttrpc.Job{sid, jid, "", transcription}

	// Call function to store
	err := TS.SetTranscription(&setjob)

	if err == nil {
		t, _ := template.ParseFiles("transcribe_submit_success.html")
		t.Execute(w, p)
	} else {
		t, _ := template.ParseFiles("transcribe_submit_failure.html")
		t.Execute(w, p)

	}
}

func handlerAddSjob(w http.ResponseWriter, r *http.Request) {
	// Get random Img from DB
	var p error
	p = nil
	t, err := template.ParseFiles("add_sjob.html")
	if err != nil {
		fmt.Println(err)
	}
	t.Execute(w, &p)
}

func handlerAddSjobSubmit(w http.ResponseWriter, r *http.Request) {
	// Get random Img from DB
	total_jobs := r.FormValue("total_jobs")
	fmt.Fprintf(w, "Total Possible Jobs:%v\n", total_jobs)

	n, err := strconv.Atoi(total_jobs)
	if err != nil {
		fmt.Fprintf(w, "Parse Total Jobs Error: %v", err)
		return
	}

	i := 0
	job_splice := make([]string, n)
	for i = 0; i < n; i++ {
		str_key := fmt.Sprintf("job:%v", i+1)

		job_details := r.FormValue(str_key)
		job_splice[i] = job_details
		if job_details == "" {
			break
		}
		fmt.Fprintf(w, "Job %v: %v\n", i+1, job_details)

	}

	// Get total truncated jobs
	n = i
	job_splice = job_splice[:n]

	err = TS.NewSuperJob(job_splice)
	if err != nil {
		fmt.Fprintf(w, "Error processing jobs: %v\n", err)
	} else {

		fmt.Fprintf(w, "Total Processed Jobs:%v\n", n)
		fmt.Fprintf(w, "Processed Jobs:%v\n", job_splice)

		fmt.Fprintf(w, "Processed Jobs!")
	}
	return
}

func handlerJS(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, r.URL.Path[1:])
	return
}

func handlerImg(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, r.URL.Path[1:])
	return
}

func printInvalidSjob(w http.ResponseWriter) {
	fmt.Fprintf(w, "<h1>Invalid ID!</h1><BR>")
}

type Job struct {
	ImgURL        string
	Transcription string
}

func viewSjob(idstr string, w http.ResponseWriter, r *http.Request) {
	defer fmt.Fprintf(w, "<p>View more sjob from the <a href=\".\">job list!</a></p>")

	id, err := strconv.Atoi(idstr)
	if err != nil {
		printInvalidSjob(w)
		return
	}

	_ = id
	// Get the details of the job
	// Do lookup

	if err != nil {
		printInvalidSjob(w)
		return
	}

	// Else print job details
	/*
	   a := Job{"http://upload.wikimedia.org/wikipedia/commons/thumb/f/f9/Wiktionary_small.svg/350px-Wiktionary_small.svg.png","W"}
	   b := Job{"https://pbs.twimg.com/profile_images/604644048/sign051.gif","GO"}
	   c := Job{"http://www.kiplinger.com/quiz/business/T049-S001-test-your-start-up-know-how/images/all-small-businesses-have-to-be-incorporated1.jpg","Inc"}

	   a := dttrpc.Job{1,1,"http://1","Trans1"}
	   b := dttrpc.Job{1,2,"http://2","Trans2"}
	   c := dttrpc.Job{1,3,"http://3","Trans3"}

	   joblist := []dttrpc.Job{ a,b,c }

	*/
	joblist, err := TS.GetSjobJobs(id)

	if err != nil {
		fmt.Fprintf(w, "Error getting sjob jobs: %v\n", err)
		return
	}

	for _, j := range joblist {
		fmt.Fprintf(w, "<div class=\"job\">")
		fmt.Fprintf(w, "<img class=\"job_img\" src=\"%v\"\\>", j.Loc)
		fmt.Fprintf(w, "<div class=\"job_transcription\">%v</div>", j.Transcript)
		fmt.Fprintf(w, "</div>")
	}

}

func viewSjobList(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "<h1>Here are the list of Sjobs:</h1>\n")

	// Get sjobs
	sjobs, err := TS.GetAllSjobs()
	if err != nil {
		fmt.Fprintf(w, "Get all Sjobs Error: %v\n", err)
	}

	for _, v := range sjobs {
		fmt.Fprintf(w, "<div><a href=\"%v\">Job: %v</a></div>", v, v)
	}

}

func handlerViewSjob(w http.ResponseWriter, r *http.Request) {

	id := r.URL.Path[len("/view_sjob/"):]
	if len(id) == 0 {
		viewSjobList(w, r)
	} else {
		viewSjob(id, w, r)
	}
	return
}

func handlerViewLog(w http.ResponseWriter, r *http.Request) {

	fmt.Fprintf(w, "<p>Index \t \t Type \t \t Struct <BR>")

	jobLog := TS.GetLog()

	for i := 0; i < len(jobLog); i++ {

		v, ok := jobLog[i]

		if !ok {
			fmt.Fprintf(w, "NO VALUE <BR>")
			continue
		}

		var vType string

		if v.Type == AddSuperJob {
			var sjob SuperJobVal

			vType = "AddSuperJob"
			json.Unmarshal(v.EncapData, &sjob)
			fmt.Fprintf(w, "%v \t \t %v \t \t %v <BR>", i, vType, sjob)

		} else if v.Type == SetTranscription {
			var trans TranscriptVal

			vType = "SetTranscription"
			json.Unmarshal(v.EncapData, &trans)
			fmt.Fprintf(w, "%v \t \t %v \t \t %v <BR>", i, vType, trans)

		} else if v.Type == AddCodeJob {
			vType = "AddNewCodeJob"
			var cjob CodeJob
			json.Unmarshal(v.EncapData, &cjob)
			fmt.Fprintf(w, "%v \t \t %v \t \t %v <BR>", i, vType, cjob)

		} else if v.Type == SetOptimize {
			vType = "SetOptimize"
			var cjob CodeJob
			json.Unmarshal(v.EncapData, &cjob)
			fmt.Fprintf(w, "%v \t \t %v \t \t %v <BR>", i, vType, cjob)

		} else {
			vType = "Noop"
			fmt.Fprintf(w, "%v \t \t %v <BR>", i, vType)
			fmt.Fprintf(w, "%v <BR>", v)
		}
	}

	fmt.Fprintf(w, "</p>")

}

func handlerGeneric(w http.ResponseWriter, r *http.Request) {

	fmt.Fprintf(w, "<body>")
	fmt.Fprintf(w, "<h1>Demo<h1>")
	fmt.Fprintf(w, "<p>Enter a key and value to submit to your storage ring.<h1>")
	// fmt.Fprintf(w, "<form action=">Key:<input type=\"text\" name=\"key\"><BR>")
	fmt.Fprintf(w, "Value:<input type=\"text\" name=\"key\"><BR>")
	fmt.Fprintf(w, "<input type=\"submit\" value=\"Submit\"></form>")
	fmt.Fprintf(w, "</body>")

	return
}

// Bool indicates success or not ,adn the int is the time if success
// If fail, the int is
// -1 - ERR
// 0 - Success
// 1 - Wrong
// 2 - Timeout
func GetCodeResults(refpath, testpath string) (int, float64) {
	cmd := exec.Command("sh", "-c", "utils/cmp_prog.sh "+refpath+" "+testpath)

	cmdout, err := cmd.Output()
	if err != nil {
		fmt.Println(err)
		return -1, 0.0
	}

	out := strings.TrimSpace(string(cmdout[:]))

	fmt.Println(out)
	if out == "TIMEOUT" {
		return 2, 0.0
	}

	if out == "WRONG" {
		return 1, 0.0
	}
	seconds_str := ""
	if len(out) > len("00:") {
		seconds_str = strings.TrimSpace(out[len("00:"):])
	} else {
		return -1, 0.0
	}
	f, ferr := strconv.ParseFloat(seconds_str, 64)
	if ferr != nil {
		fmt.Println(ferr)
		return -1, 0.0
	}
	return 0, f
}

func printInvalidCodeJob(w http.ResponseWriter) {
	fmt.Fprintf(w, "<h1>Invalid ID!</h1><BR>")
}

type EditCode struct {
}

func viewCodeJob(idstr string, w http.ResponseWriter, r *http.Request) {
	defer fmt.Fprintf(w, "<p>View more Code Snippets from the <a href=\".\">Code list!</a></p>")

	id, err := strconv.Atoi(idstr)
	if err != nil {
		printInvalidCodeJob(w)
		return
	}

	_ = id
	// Get the details of the job
	// Do lookup

	if err != nil {
		printInvalidCodeJob(w)
		return
	}

	// Else print job details
	codejob, err := TS.GetCode(id)

	// Get title
	if err != nil {
		fmt.Fprintf(w, "Error getting Code Job: %v\n", err)
		return
	}

	if codejob.Name == "" {
		codejob.Name = "<NO SUBMISSION YET>"
		codejob.Time = 0.0
	}

	/* TODO: INITIALIZE P */
	t, err := template.ParseFiles("edit_code.html")
	if err != nil {
		fmt.Println(err)
	}
	t.Execute(w, &codejob)
}

func viewCodeJobs(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "<h1>Here are the list of Codes to Optimize!</h1>\n")

	// Get Code Jobs
	sjobs, err := TS.GetAllCodeJobs()
	if err != nil {
		return
		fmt.Fprintf(w, "Get all Code Jobs Error: %v\n", err)
	}

	// Change this
	for _, v := range sjobs {
		fmt.Fprintf(w, "<div><a href=\"%v\">Job %v: %v</a></div>", v.Jid, v.Jid, v.Title)
	}

}

func handlerViewCodeJob(w http.ResponseWriter, r *http.Request) {

	id := r.URL.Path[len("/view_code/"):]
	if len(id) == 0 {
		viewCodeJobs(w, r)
	} else {
		viewCodeJob(id, w, r)
	}
	return
}

func handlerAddCode(w http.ResponseWriter, r *http.Request) {
	// Get random Img from DB
	var p error
	p = nil
	t, err := template.ParseFiles("add_code.html")
	if err != nil {
		fmt.Println(err)
	}
	t.Execute(w, &p)
}

func handlerAddCodeSubmit(w http.ResponseWriter, r *http.Request) {
	// Get random Img from DB
	title := r.FormValue("title")
	code := r.FormValue("code")

	// Create struct to put in set optimize

	err := TS.NewCodeJob(code, title)

	var html string
	if err != nil {
		html = "add_code_fail.html"
	} else {
		html = "add_code_success.html"
	}

	var p error
	p = nil
	t, err := template.ParseFiles(html)
	if err != nil {
		fmt.Println(err)
	}
	t.Execute(w, &p)

	return
}

type OptSub struct {
	Status string
}

func handlerCodeOptimizeSubmit(w http.ResponseWriter, r *http.Request) {
	// Get random Img from DB
	id_str := r.URL.Path[len("/code_submit/"):]
	id, err := strconv.Atoi(id_str)
	if err != nil {
		printInvalidCodeJob(w)
		return
	}
	name := r.FormValue("name")
	submit_code := r.FormValue("submit_code")
	fmt.Printf("EOFKOPS : %v\n\n", submit_code)

	var setjob CodeJob
	setjob = CodeJob{Jid: id, Title: "", OldCode: "", NewCode: submit_code, Name: name, Time: 0.0}

	// Call function to store
	err = TS.SetOptimize(&setjob)
	var p OptSub
	if err == nil {
		p.Status = "Processing!"
	} else {
		p.Status = err.Error()
	}

	t, _ := template.ParseFiles("view_code_submit.html")
	t.Execute(w, &p)
}

func (ts *dttServer) GetCode(codeID int) (*CodeJob, error) {

	var Title string
	var OldCode string
	var NewCode string
	var Name string
	var Time float64
	var err error

	v := strconv.Itoa(codeID)
	Title, err = ts.Lstore.Get(TITLE_PREFIX + v)
	if err != nil {
		return nil, err
	}

	OldCode, err = ts.Lstore.Get(OLDCODE_PREFIX + v)
	if err != nil {
		return nil, err
	}

	NewCode, err = ts.Lstore.Get(NEWCODE_PREFIX + v)
	if err != nil {
		return nil, err
	}

	Name, err = ts.Lstore.Get(NAME_PREFIX + v)
	if err != nil {
		return nil, err
	}

	TimeStr, err := ts.Lstore.Get(TIME_PREFIX + v)
	if err != nil {
		return nil, err
	}
	Time, err = strconv.ParseFloat(TimeStr, 64)
	if err != nil {
		return nil, err
	}

	job := new(CodeJob)
	job.Jid = codeID
	job.Title = Title
	job.OldCode = OldCode
	job.NewCode = NewCode
	job.Name = Name
	job.Time = Time

	return job, nil
}

func (ts *dttServer) SetOptimize(code *CodeJob) error {

	var err error

	key := strconv.Itoa(code.Jid)

	// check whether a transcription exists
	_, founderr := ts.Lstore.Get(OLDCODE_PREFIX + key)
	if founderr != nil {
		return errors.New("ERROR: Job does not exist")
	}

	curTime, err := ts.Lstore.Get(TIME_PREFIX + key)

	// Get Code Results and proces
	refpath := REF_PREFIX + strconv.Itoa(code.Jid) + ".py"
	testpath := fmt.Sprintf(TEST_PREFIX+"%v.py", time.Now())
	testpath = strings.Replace(testpath, " ", "", -1)

	// Put in code into test path
	err = ioutil.WriteFile(testpath, []byte(code.NewCode), os.ModePerm)
	if err != nil {
		return err
	}

	status, testtime := GetCodeResults(refpath, testpath)

	//status, testtime:= GetCodeResults(testpath, testpath)
	fmt.Printf("Satatus: %v\n", status)
	switch status {
	case 1:
		return errors.New("Wrong Output")
	case 2:
		return errors.New("Timeout")
	case -1:
		return errors.New("Script Error")
	}

	// else we assert iti s 0
	code.Time = testtime
	curTimeFloat, _ := strconv.ParseFloat(curTime, 64)

	if err == nil && code.Time < curTimeFloat {
		// if there is a time in the datastore and it is worse than the new code's time,
		// run paxos on the value
		tryOptim := new(LogEntry)
		tryOptim.Type = SetOptimize
		tryOptim.EncapData, err = json.Marshal(code)

		return ts.NewLogEntry(tryOptim)

	} else {
		// if newTimeFloat < curTimeFloat was false, error will be nil
		// if err != nil, return this error

		return errors.New("Not quite as fast as da current optimized code!")
	}
	return nil
}

func (ts *dttServer) SaveOptimize(code *CodeJob) error {

	var err error

	key := strconv.Itoa(code.Jid)

	// check whether a transcription exists
	_, founderr := ts.Lstore.Get(OLDCODE_PREFIX + key)

	if founderr != nil {
		return errors.New("ERROR: Job does not exist")
	}

	curTime, err := ts.Lstore.Get(TIME_PREFIX + key)

	curTimeFloat, _ := strconv.ParseFloat(curTime, 64)

	if err == nil && code.Time < curTimeFloat {
		// if there is a time in the datastore and it is worse than the new code's time,
		// store the new code

		timeStr := fmt.Sprintf("%.2f", code.Time)

		puterr := ts.Lstore.Put(TIME_PREFIX+key, timeStr)

		if puterr != nil {
			return puterr
		}

		puterr = ts.Lstore.Put(NAME_PREFIX+key, code.Name)

		if puterr != nil {
			return puterr
		}

		puterr = ts.Lstore.Put(NEWCODE_PREFIX+key, code.NewCode)

		if puterr != nil {
			return puterr
		}

	} else {
		// if newTimeFloat < curTimeFloat was false, error will be nil
		// if err != nil, return this error
		return err
	}
	return nil
}
