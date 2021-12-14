/* deanraccoon@gmail.com */
/* vim: set ts=4 shiftwidth=4 smarttab noet : */

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/codegangsta/martini"
	"github.com/largbae/radoshttpd/rados"
	"github.com/largbae/radoshttpd/nettimeout"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"crypto/subtle"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"encoding/hex"
	"container/list"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"bufio"
	"github.com/wenjianhn/chunkaligned"
	"github.com/largbae/wugui"
)

var (
	LOGPATH                    = "/var/log/wuzei/wuzei.log"
	PIDFILE                    = "/var/run/wuzei/wuzei.pid"
	WHITELISTPATH              = "/etc/wuzei/whitelist"
	QUEUETIMEOUT time.Duration = 5 /* seconds */
	MONTIMEOUT                 = "30"
	OSDTIMEOUT                 = "60"
	BUFFERSIZE                 = 4 << 20 /* 4M */
	AIOCONCURRENT              = 4
	MAX_CHUNK_SIZE             = BUFFERSIZE * 2

	STRIPE_UNIT		   = uint(512 << 10) /* 512K */
	OBJECT_SIZE                = uint(4 << 20) /* 4M */
	STRIPE_COUNT               = uint(4)
	SECRET                     = ""

	/* global variables */
	slog  *log.Logger
	ReqQueue RequestQueue
	wg       sync.WaitGroup
	/* IP address which could access anytime */
	/* ignore any line prefixed by # */
	whiteList map[string]int
	/* blocked object list */
	blackList *SafeMap
	urlRecord *URLRecord
)


type Record struct {
	last_access_time time.Time
	num_of_access int
	pos_in_list *list.Element
}

type URLRecord struct {
	records map[string] *Record
	lock * sync.Mutex
	evictList *list.List
	max_record_size int
}


// From https://github.com/codegangsta/martini-contrib/blob/master/auth/util.go
// SecureCompare performs a constant time compare of two strings to limit timing attacks.
func SecureCompare(given string, actual string) bool {
        if subtle.ConstantTimeEq(int32(len(given)), int32(len(actual))) == 1 {
                return subtle.ConstantTimeCompare([]byte(given), []byte(actual)) == 1
        } else {
                /* Securely compare actual to itself to keep constant time, but always return false */
                return subtle.ConstantTimeCompare([]byte(actual), []byte(actual)) == 1 && false
        }
}



func NewURLRecord() *URLRecord {
	r := make(map[string] *Record)
	l := new(sync.Mutex)
	e := list.New()
	size := 1000

	return &URLRecord{r,l,e,size}
}

func (url_record *URLRecord) update_and_check(url string) bool {
	url_record.lock.Lock()
	defer url_record.lock.Unlock()
	var we_are_attacked bool = false
	var entry *Record
	var ok bool
	current_time := time.Now()
	//Has record
	if entry, ok = url_record.records[url]; ok {
		//new request within 10s
		if entry.last_access_time.Add(time.Duration(cfg.ThrottleInterval) * time.Second).After(current_time)  {
			entry.num_of_access += 1
			if entry.num_of_access > cfg.ThrottleNums {
				we_are_attacked = true
			}
		//new request outof 10s, re-caculate the time
		} else {
			entry.num_of_access = 1
			entry.last_access_time = current_time
		}
		//update ElevictList
		url_record.evictList.MoveToFront(entry.pos_in_list)
	//Add new record 
	} else {
		new_entry := &Record{current_time, 1, nil}
		var pos *list.Element = url_record.evictList.PushFront(url)
		//After new_entry is inserted into list, put its pointer to entry it self
		new_entry.pos_in_list = pos
		url_record.records[url] = new_entry
	}

	//evicte old map
	if url_record.evictList.Len() > url_record.max_record_size {
		i := 0
		for i < url_record.max_record_size / 2 {
			element := url_record.evictList.Back()
			url = url_record.evictList.Remove(element).(string)
			slog.Printf("evict old entry %s", url)
			delete(url_record.records, url)
			i ++
		}
	}

	return we_are_attacked;
}

type RadosDownloader struct {
	striper       *rados.StriperPool
	soid          string
	offset        int64
	buffer        []byte
	waterhighmark int
	waterlowmark  int
}

type SimpleRadosDownloader struct {
	*RadosDownloader
}

func (rd *SimpleRadosDownloader) Read(p []byte) (n int, err error) {
	count, err := rd.RadosDownloader.striper.Read(rd.RadosDownloader.soid, p, uint64(rd.RadosDownloader.offset))
	if count == 0 {
		return 0, io.EOF
	}
	rd.RadosDownloader.offset += int64(count)
	return count, err
}

func (rd *SimpleRadosDownloader) Seek(offset int64, whence int) (int64, error) {
	return rd.RadosDownloader.Seek(offset, whence)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (rd *RadosDownloader) Read(p []byte) (n int, err error) {
	var count int = 0
	/* local buffer is empty */
	if rd.waterhighmark == rd.waterlowmark {
		count, err = rd.striper.Read(rd.soid, rd.buffer, uint64(rd.offset))
		if count == 0 {
			return 0, io.EOF
		}
		/* Timeout or read error occurs */
		if err != nil {
			slog.Println("timeout when reading ceph object");
			return count, errors.New("Timeout or Read Error")
		}
		rd.offset += int64(count)
		rd.waterhighmark = count
		rd.waterlowmark = 0
	}

	l := len(p)
	if l <= rd.waterhighmark-rd.waterlowmark {
		copy(p, rd.buffer[rd.waterlowmark:rd.waterlowmark+l])
		rd.waterlowmark += l
		return l, nil
	} else {
		copy(p, rd.buffer[rd.waterlowmark:rd.waterhighmark])
		rd.waterlowmark = rd.waterhighmark
		return rd.waterhighmark - rd.waterlowmark, nil
	}

}

func (rd *RadosDownloader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		rd.offset = offset
		return offset, nil
	case 1:
		rd.offset += offset
		return rd.offset, nil
	case 2:
		size, _, err := rd.striper.State(rd.soid)
		if err != nil {
			return 0, nil
		}
		rd.offset = int64(size)
		return rd.offset, nil
	default:
		return 0, errors.New("failed to seek")
	}

}

/* RequestQueue has two functions */
/* 2. slot is used to queue write/read request */
type RequestQueue struct {
	slots chan bool
}

func (r *RequestQueue) Init(queueLength int) {
	r.slots = make(chan bool, queueLength)
}

func (r *RequestQueue) inc() error {
	select {
	case <-time.After(QUEUETIMEOUT * time.Second):
		return errors.New("Queue is too long, timeout")
	case r.slots <- true:
		/* write to channel to get a slot for writing/reading rados object */
	}
	return nil
}

func (r *RequestQueue) dec() {
	<-r.slots
}

func (r *RequestQueue) size() int {
	return len(r.slots)
}

//use HMAC
func AuthMe(key string) martini.Handler {
	return func(res http.ResponseWriter, req *http.Request, c martini.Context) {
		/* allow all GET */
		if req.Method == "GET" || req.Method == "HEAD" {
			return
		}
		auth := req.Header.Get("Authorization")
		mac := hmac.New(sha1.New, []byte(key))
		mac.Write([]byte(req.URL.Path))
		expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if !SecureCompare(auth, expected) {
			slog.Println("URL:", req.URL, "expected key is %s, but received key is %s", expected, auth);
			ErrorHandler(res, req, http.StatusUnauthorized)
		}
	}
}



func WrapBytesCounter() martini.Handler {
	return func(res http.ResponseWriter, req *http.Request, c martini.Context) {
		r := &BytesCounter{0}
		c.Map(r)
	}
}


//any object is retrived too many times, block all the request except whiteList
//return false to let request go
//return true to block request
func DDosProtect(w http.ResponseWriter, r *http.Request, conn *rados.Conn) bool{

		remote_addr_parts := strings.Split(r.RemoteAddr, ":")
		remote_addr := remote_addr_parts[0]

		if _, ok := whiteList[remote_addr]; ok{
			return false
		}

		url := r.RequestURI
		if blackList.Check(url) {
			rejectConnection(w)
			return true
		} else if urlRecord.update_and_check(url){
			blackList.Set(url, time.Now())
			rejectConnection(w)
			return true
		} else {
			return false
		}
}

func rejectConnection(w http.ResponseWriter){
		hj, ok := w.(http.Hijacker)
		if !ok {
			slog.Println("webserver doesn't support hijacking")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		conn.Close()
}

func isCacheable(size int64) bool {
	// NOTE(wenjianhn): only cache small files now
	// We may need to cache file whose name contains specific suffix/characters.

	if size <= int64(cfg.CacheMaxObjectSizeKBytes) * 1024 {
		return true
	}
	return false
}



func RequestLimit() martini.Handler {
	return func(w http.ResponseWriter, r *http.Request, c martini.Context){
        /* used for graceful stop */
		wg.Add(1)
		defer wg.Done()

        /* limit the max request */
		if err := ReqQueue.inc(); err != nil {
			slog.Println("URL:", r.URL, ", request timeout")
			ErrorHandler(w, r, http.StatusRequestTimeout)
			return
		}
		defer ReqQueue.dec()
		c.Next()
	}
}

func GetHandler(params martini.Params, w http.ResponseWriter, r *http.Request, conn *rados.Conn, counter *BytesCounter) {
    
	if cfg.DDos && DDosProtect(w,r,conn) {
		slog.Printf("See %s, Blacklist it", r.URL)
		return
	}


	poolname := params["pool"]
	soid := params["soid"]
	pool, err := conn.OpenPool(poolname)
	if err != nil {
		slog.Println("URL:", r.URL, "open pool failed")
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	defer pool.Destroy()

	striper, err := pool.CreateStriper()
	if err != nil {
		slog.Println("URL:", r.URL, "Create Striper failed")
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	defer striper.Destroy()

	filename := fmt.Sprintf("%s", soid)
	size, _, err := striper.State(soid)
	if err != nil {
		slog.Println("URL:", r.URL, "failed to get object " + soid)
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}

	if isCacheable(int64(size)) {
		rr := wugui.NewRadosReaderAt(&striper, poolname, filename, int64(size))
		content, err := chunkaligned.NewChunkAlignedReaderAt(&rr, cfg.CacheChunkSizeKBytes * 1024)
		if err != nil {
			slog.Println(err)
			ErrorHandler(w, r, http.StatusInternalServerError)
			return
		}

		readSeeker := io.NewSectionReader(content, 0, content.Size())
		ServeContent(w, r, filename, readSeeker, counter)
		return
    }

	rd := RadosDownloader{&striper, soid, 0, nil, 0, 0}
	srd := &SimpleRadosDownloader{&rd}

	/* set let ServerContent to detect file type  */
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))

	/* set the stream */
    /* here we need more data */
	ServeContent(w, r, filename, srd, counter)
}

func BlockHandler(params martini.Params, w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(fmt.Sprintf("{\"blocksize\":%d}", MAX_CHUNK_SIZE * AIOCONCURRENT)))
}


func Md5sumHandler(params martini.Params, w http.ResponseWriter, r *http.Request, conn *rados.Conn) {

	poolname := params["pool"]
	soid := params["soid"]
	pool, err := conn.OpenPool(poolname)
	if err != nil {
		slog.Println("URL:", r.URL, "open pool failed")
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	defer pool.Destroy()

	striper, err := pool.CreateStriper()
	if err != nil {
		slog.Println("URL:", r.URL, "Create Striper failed")
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	defer striper.Destroy()
	defer striper.Flush()


	var offset int64 = 0
	var start, end int64 = 0, 0
	var count,l int = 0, 0

	/* header format: Range:bytes 0-99 */
	bytesRange := r.Header.Get("Range")
	if bytesRange != "" {
		bytesRange = strings.Trim(bytesRange, "bytes")
		bytesRange = strings.TrimSpace(bytesRange)
		o := strings.Split(bytesRange, "-")
		start, err = strconv.ParseInt(o[0], 10, 64)
		if err != nil {
			slog.Println("URL:", r.URL, "parse Content-Range failed %s", bytesRange)
			ErrorHandler(w, r, http.StatusBadRequest)
			return
		}
		end, err = strconv.ParseInt(o[1], 10, 64)
		if err != nil {
			slog.Println("URL:", r.URL, "parse Content-Range failed %s", bytesRange)
			ErrorHandler(w, r, http.StatusBadRequest)
			return
		}
		offset = start
	}


	md5_ctx,_ := MD5New()
	buf := make([]byte, BUFFERSIZE)
	for offset <= end || bytesRange == "" {
		count, err = striper.Read(soid, buf, uint64(offset))
		if err != nil {
			slog.Println("URL:", r.URL, "failed to read data for md5sum")
			ErrorHandler(w, r, 404)
			return
		}
		if count == 0 {
			break
		}

		/* Handle striper.read more data than expected when having Range Header*/
		if bytesRange != "" && offset + int64(count) > end {
			l = int(end - offset) + 1
		} else {
			l = count
		}

		if err = md5_ctx.Update(buf[:l]); err != nil {
			slog.Println("URL:", r.URL, "calc md5sum failed")
			ErrorHandler(w, r, 501)
			return
		}
		offset += int64(count)
	}

	var b []byte
	if b, err = md5_ctx.Final(); err != nil {
		slog.Println("URL:", r.URL, "calc md5sum failed")
		ErrorHandler(w, r, 501)
		return
	}

	s := hex.EncodeToString(b)
	w.Write([]byte(fmt.Sprintf("{\"md5\":\"%s\"}", s)))
}

func CephStatusHandler(params martini.Params, w http.ResponseWriter, r *http.Request, conn *rados.Conn) {
    c, err := conn.Status()
    if err != nil{
		    ErrorHandler(w, r, 504)
            return
    }
	w.Write([]byte(c))
}

func InfoHandler(params martini.Params, w http.ResponseWriter, r *http.Request, conn *rados.Conn) {
	poolname := params["pool"]
	soid := params["soid"]
	pool, err := conn.OpenPool(poolname)
	if err != nil {
		slog.Println("URL:", r.URL, "open pool failed")
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	defer pool.Destroy()

	striper, err := pool.CreateStriper()
	if err != nil {
		slog.Println("URL:", r.URL, "Create Striper failed")
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	defer striper.Destroy()

	size, _, err := striper.State(soid)
	if err != nil {
		slog.Println("URL:%s, failed to get object " + soid, r.URL)
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	/* use json format */
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(fmt.Sprintf("{\"size\":%d}", size)))
	return
}

func DeleteHandler(params martini.Params, w http.ResponseWriter, r *http.Request, conn *rados.Conn) {

	poolname := params["pool"]
	soid := params["soid"]
	pool, err := conn.OpenPool(poolname)
	if err != nil {
		slog.Println("URL:", r.URL, "open pool failed")
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	defer pool.Destroy()

	striper, err := pool.CreateStriper()
	if err != nil {
		slog.Println("URL:", r.URL, "Create Striper failed")
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	defer striper.Destroy()
	err = striper.Delete(soid)
	if err != nil {
		slog.Println("URL:", r.URL, "delete object %s/%s failed\n", poolname, soid)
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

/* Function name 'pending_has_completed' and 'wait_pending_front' are the same as the radosgw  */
func pending_has_completed(p *list.List) bool{
	if p.Len() == 0 {
		return false
	}
	e := p.Front()
	c := e.Value.(*rados.AioCompletion)
	ret := c.IsComplete()
	if ret == 0 {
		return false
	} else {
		return true
	}
}


func wait_pending_front(p * list.List) int{
	/* remove AioCompletion from list */
	e := p.Front()
	p.Remove(e)
	c := e.Value.(*rados.AioCompletion)
	c.WaitForComplete()
	ret := c.GetReturnValue()
	c.Release()
	return ret
}


func drain_pending(p * list.List) int {
	var ret int
	for p.Len() > 0 {
		ret = wait_pending_front(p)
	}
	return ret
}


func set_stripe_layout(p * rados.StriperPool) int{
	var ret int = 0
	if ret = p.SetLayoutStripeUnit(STRIPE_UNIT); ret < 0 {
		return ret
	}
	if ret = p.SetLayoutObjectSize(OBJECT_SIZE); ret < 0 {
		return ret
	}
	if ret = p.SetLayoutStripeCount(STRIPE_COUNT); ret < 0 {
		return ret
	}
	return ret
}

func PutHandler(params martini.Params, w http.ResponseWriter, r *http.Request, conn *rados.Conn, ) {
	poolname := params["pool"]
	soid := params["soid"]
	pool, err := conn.OpenPool(poolname)
	if err != nil {
		slog.Println("URL:", r.URL, "open pool failed")
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	defer pool.Destroy()
	striper, err := pool.CreateStriper()
	if err != nil {
		slog.Println("URL:", r.URL, "Create Striper failed")
		ErrorHandler(w, r, http.StatusNotFound)
		return
	}
	defer striper.Destroy()
	set_stripe_layout(&striper)

	bytesRange := r.Header.Get("Content-Range")

	var src_offset, dest_offset, start, end, size int64 = 0, 0, 0, 0, 0

	if bytesRange != "" {
		/* header format: Content-Range:bytes 0-99/300 */
		/* remove bytes and space */
		bytesRange = strings.Trim(bytesRange, "bytes")
		bytesRange = strings.TrimSpace(bytesRange)

		o := strings.Split(bytesRange, "/")
		currentRange, s := o[0], o[1]

		o = strings.Split(currentRange, "-")

		/* o[0] is the start, o[1] is the end */
		start, err = strconv.ParseInt(o[0], 10, 64)
		if err != nil {
			slog.Println("URL:", r.URL, "parse Content-Range failed %s", bytesRange)
			ErrorHandler(w, r, http.StatusBadRequest)
			return
		}
		end, err = strconv.ParseInt(o[1], 10, 64)
		if err != nil {
			slog.Println("URL:", r.URL, "parse Content-Range failed %s", bytesRange)
			ErrorHandler(w, r, http.StatusBadRequest)
			return
		}

		size, err = strconv.ParseInt(s, 10, 64)
		if err != nil {
			slog.Println("URL:", r.URL, "parse Content-Range failed %s", bytesRange)
			ErrorHandler(w, r, http.StatusBadRequest)
			return
		}
	}


	if bytesRange != "" {
		/* already get $start, $end */
		src_offset = start
		dest_offset = start
	} else {
		src_offset = 0
		dest_offset = 0
	}


	buf := make([]byte, BUFFERSIZE)
	/* if the data len in pending_data is bigger than MAX_CHUNK_SIZE, I will flush the data to ceph */
	var pending_data []byte
	var available_data_size int
	var c  *rados.AioCompletion
	pending := list.New()

	for src_offset <= end || bytesRange == "" {

		count, err := r.Body.Read(buf)
		if count == 0 {
			break
		}
		if err != nil && err != io.EOF {
				slog.Printf("failed to read content from client url:%s, %s",
					r.RequestURI, err.Error())
				drain_pending(pending)
				ErrorHandler(w, r, http.StatusBadRequest)
				return
		}

		//In case the user send more data than expected.
		if bytesRange != "" {
			available_data_size = min(count, int(end - src_offset + 1))
		} else {
			available_data_size = count
		}
		src_offset += int64(count)

		/* add newly received buffer to pending_data */
		pending_data = append(pending_data, buf[:available_data_size]...)

		/* if pending_data is not big enough, continue to read more data */
		if len(pending_data) < MAX_CHUNK_SIZE {
			continue
		}

		/* will write bl to ceph */
		bl := pending_data[:MAX_CHUNK_SIZE]
		/* now pending_data point to remaining data */
		pending_data = pending_data[MAX_CHUNK_SIZE:]


		c = new(rados.AioCompletion)
		c.Create()
		_, err = striper.WriteAIO(c, soid, bl, uint64(dest_offset))
		if err != nil {
			slog.Println("URL:", r.URL, "starting to write aio failed")
			c.Release()
			drain_pending(pending)
			ErrorHandler(w, r, 501)
			return
		}
		pending.PushBack(c)

		//Throttle data
		//if the front is finished, cleanup
		for pending_has_completed(pending) {
			if ret := wait_pending_front(pending); ret < 0 {
				slog.Println("URL:%s, write aio failed or timeout, in pending_has_completed", r.RequestURI)
				drain_pending(pending)
				ErrorHandler(w, r, 408)
				return
			}
		}

		if pending.Len() > AIOCONCURRENT {
			slog.Println("inputstream is a bit faster, wait to finish")
			if ret := wait_pending_front(pending); ret < 0 {
				slog.Println("URL:%s, write aio failed or timeout, in waiting pending ", r.RequestURI)
				drain_pending(pending)
				ErrorHandler(w, r, 408)
				return
			}
		}

		dest_offset += int64(len(bl))
	}

	//write all remaining data
	if len(pending_data) > 0 {
		c = new(rados.AioCompletion)
		c.Create()
		striper.WriteAIO(c, soid, pending_data, uint64(dest_offset))
		pending.PushBack(c)
	}


	//drain_pending
	if ret := drain_pending(pending); ret < 0 {
		slog.Println("URL:%s, write aio failed or timeout, in draining", r.RequestURI)
		ErrorHandler(w, r, 408)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")

	if bytesRange == "" {
		w.WriteHeader(http.StatusOK)
		return
	} else {
		/* user send too much data or right data*/
		if (src_offset >= end) {
			w.Header().Set("Range", fmt.Sprintf("%d-%d/%d", start, end, size))
		} else {
			/* user send too few data */
			w.Header().Set("Range", fmt.Sprintf("%d-%d/%d", start, src_offset - 1, size))
		}
		w.WriteHeader(http.StatusOK)
	}
}

type gcCfg struct {
	Name string
	CacheSizeMBytes int   // max per-node memory usage
	CacheChunkSizeKBytes int
	CacheMaxObjectSizeKBytes int  // Defines maximum size of the s3 object to be kept in cache.
	MyIPAddr string
	Port int
	Peers []string  // IP addresses of all the nodes
	ListenPort int
	SocketTimeout int
	QueueLength int
	SecretKey string
	DDos bool
	ThrottleInterval int
	ThrottleNums int
}

var cfg gcCfg

func getGcCfg() (cfg gcCfg, err error) {
	// TODO(wenjianhn): get json from etcd

	f, err := os.Open("/etc/wuzei/wuzei.json")
	if err != nil {
		slog.Println("Parse wuzei.json failed")
		return
	}
	defer f.Close()

	err = json.NewDecoder(f).Decode(&cfg)
	if err != nil {
		err = errors.New("failed to parse wuzei.json: " + err.Error())
		slog.Println("Parse wuzei.json failed")
		return
	}

	found := false
	for _, peer := range cfg.Peers {
		if peer == cfg.MyIPAddr {
			found = true
			break
		}
	}
	if !found {
		cfg.Peers = append(cfg.Peers, cfg.MyIPAddr)
	}
	SECRET = cfg.SecretKey

	if cfg.DDos {
		slog.Printf("Support DDos protect, any object will be blocked when accessing %d in %d seconds", cfg.ThrottleNums, cfg.ThrottleInterval)
	} else {
		slog.Printf("No DDos protect")

	}

	return
}


type BytesCounter struct {
	byteSend int64
}

func main() {

	var conn  *rados.Conn

	/* log  */
	f, err := os.OpenFile(LOGPATH, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		fmt.Println("failed to open log\n")
		return
	}
	defer f.Close()


	m := martini.Classic()
	slog = log.New(f, "[wuzei]", log.LstdFlags)


    //Redirect stdout and stderr to the log
    syscall.Dup2(int(f.Fd()), 2)
    syscall.Dup2(int(f.Fd()), 1)


	cfg, err = getGcCfg()
	if err != nil {
		slog.Println(err.Error())
		return
	}

	if cfg.DDos {
		whiteList = make(map[string]int)
		// initial white list
		f_whitelist, err := os.OpenFile(WHITELISTPATH, os.O_RDONLY, 0666)
		if err != nil {
			fmt.Println("failed to open whitelist")
			f_whitelist.Close()
		}
		scanner := bufio.NewScanner(f_whitelist)
		var line string
		for scanner.Scan() {
			line = scanner.Text()
			//put whitelist in map
			slog.Printf("Put %s into whitelist", line)
			whiteList[line] = 0
		}
		f_whitelist.Close()

		blackList = NewSafeMap()
		urlRecord = NewURLRecord()

		//if blacklist is long, it will be slow
		go func(){
			//v is the inserted time
			current_time := time.Now()
			for {
				time.Sleep(time.Minute * 60)
				for k, v := range blackList.Items() {
					if v.(time.Time).Add(time.Minute * 60).After(current_time) {
						blackList.Delete(k)
					}
				}
			}
		}()
	}

	m.Use(AuthMe(SECRET))
	m.Use(WrapBytesCounter())
	m.Use(func(w http.ResponseWriter, r *http.Request, conn *rados.Conn, counter *BytesCounter, c martini.Context){
    start := time.Now()
	addr := r.Header.Get("X-Real-IP")
	if addr == "" {
		addr = r.Header.Get("X-Forwarded-For")
	if addr == "" {
		addr = r.RemoteAddr
	}
	c.Next()
    rw := w.(martini.ResponseWriter)
    slog.Printf("COMPLETE %s %s %s %v %d in %s\n", addr, r.Method, r.URL.Path, rw.Status(), counter.byteSend, time.Since(start))
    }})


	wugui.InitCachePool(cfg.MyIPAddr, cfg.Peers, cfg.Port)
	slog.Printf("Config of group cache: %+v\n", cfg)

	var cacheSize int64
	cacheSize = int64(cfg.CacheSizeMBytes) * 1024 * 1024
	wugui.InitRadosCache(cfg.Name, cacheSize, cfg.CacheChunkSizeKBytes * 1024)

	conn, err = rados.NewConn("admin")
	if err != nil {
		slog.Println("failed to open keyring")
		return
	}

	conn.SetConfigOption("rados_mon_op_timeout", MONTIMEOUT)
	conn.SetConfigOption("rados_osd_op_timeout", OSDTIMEOUT)

	err = conn.ReadConfigFile("/etc/ceph/ceph.conf")
	if err != nil {
		slog.Println("failed to open ceph.conf")
		return
	}

	err = conn.Connect()
	if err != nil {
		slog.Println("failed to connect to remote cluster")
		return
	}
	defer conn.Shutdown()

    m.Map(conn)

	ReqQueue.Init(cfg.QueueLength)

	m.Get("/whoareyou", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("I AM WUZEI"))
	})

	m.Get("/cachestats", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, fmt.Sprintf("%+v\n", wugui.GetRadosCacheStats()))
	})

	m.Get("/threads", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, fmt.Sprintf("%d\n", ReqQueue.size()))
	})

	/* resume upload protocal is based on http://www.grid.net.ru/nginx/resumable_uploads.en.html */
	m.Put("/(?P<pool>[A-Za-z0-9]+)/(?P<soid>[^/]+)", RequestLimit(), PutHandler)
	m.Delete("/(?P<pool>[A-Za-z0-9]+)/(?P<soid>[^/]+)", RequestLimit(), DeleteHandler)
	m.Get("/(?P<pool>[A-Za-z0-9]+)/(?P<soid>[^/]+)", RequestLimit(), GetHandler)
	m.Get("/info/(?P<pool>[A-Za-z0-9]+)/(?P<soid>[^/]+)", RequestLimit(), InfoHandler)
	m.Get("/calcmd5/(?P<pool>[A-Za-z0-9]+)/(?P<soid>[^/]+)", RequestLimit(), Md5sumHandler)
	m.Get("/blocksize",BlockHandler)
	m.Get("/cephstatus",CephStatusHandler)

	sl, err := nettimeout.NewListener(cfg.ListenPort, time.Duration(cfg.SocketTimeout) * time.Second,
						time.Duration(cfg.SocketTimeout) * time.Second);
	if err != nil {
		fmt.Printf("Failed to listen to %d, quiting\n", cfg.ListenPort)
		os.Stdout.Sync()
		slog.Printf("Failed to listen to %d, quiting", cfg.ListenPort)
		return
	}

	server := http.Server{}
	http.HandleFunc("/", m.ServeHTTP)

	sigChan := make(chan os.Signal)
	signal.Notify(sigChan, syscall.SIGINT,
		syscall.SIGHUP,
		syscall.SIGQUIT,
		syscall.SIGTERM)

	go func() {
		server.Serve(sl)
	}()

	slog.Printf("Serving HTTP\n")
	for {
		select {
		case signal := <-sigChan:
			slog.Printf("Got signal:%v\n", signal)
			switch signal {
			case syscall.SIGHUP:
				slog.Println("Reloading config file.")
				cfg, err = getGcCfg()
				if err != nil {
					slog.Printf("Failed to load config: %s\n", err.Error())
					continue
				}
				slog.Printf("Updating Peers to: %v with Port: %d\n", cfg.Peers, cfg.Port)
				wugui.SetCachePoolPeers(cfg.Peers, cfg.Port)
			default:
				sl.Stop()
				slog.Printf("Waiting on server\n")
				wg.Wait()
				slog.Printf("Server shutdown\n")

				// NOTE(wenjianhn): deferred functions will not run if using os.Exit(0).
				return
			}
		}
	}
}


func ErrorHandler(w http.ResponseWriter, r *http.Request, status int) {
	switch status {
	case http.StatusForbidden:
		w.WriteHeader(status)
		w.Write([]byte("Forbidden"))
	case http.StatusNotFound:
		w.WriteHeader(status)
		w.Write([]byte("object not found"))
	case http.StatusRequestTimeout:
		w.WriteHeader(status)
		w.Write([]byte("server is too busy,timeout"))
	case http.StatusUnauthorized:
		w.WriteHeader(status)
		w.Write([]byte("UnAuthorized"))
	case http.StatusInternalServerError:
		w.WriteHeader(status)
		w.Write([]byte("Internal Server Error"))
	default:
		w.WriteHeader(status)
		w.Write([]byte("error"))
	}
}
