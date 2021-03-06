
package main

import (
	os "os"
	io "io"
	time "time"
	sort "sort"
	strings "strings"
	strconv "strconv"
	fmt "fmt"
	sync "sync"
	context "context"
	tls "crypto/tls"
	http "net/http"

	ufile "github.com/KpnmServer/go-util/file"
	json "github.com/KpnmServer/go-util/json"
)

type Cluster struct{
	host string
	public_port uint16
	username string
	password string
	version string
	useragent string
	prefix string

	cachedir string
	hits uint32
	hbytes uint64
	max_conn uint
	issync bool

	enabled bool
	socket *Socket
	connlock sync.Mutex
	keepalive func()(bool)

	client *http.Client
	Server *http.Server
}

func NewCluster(
	host string, public_port uint16,
	username string, password string,
	version string, address string)(cr *Cluster){
	cr = &Cluster{
		host: host,
		public_port: public_port,
		username: username,
		password: password,
		version: version,
		useragent: "openbmclapi-cluster/" + version,
		prefix: "https://openbmclapi.bangbang93.com",

		cachedir: "cache",
		hits: 0,
		hbytes: 0,
		max_conn: 400,
		issync: false,

		enabled: false,
		socket: nil,
		connlock: sync.Mutex{},

		client: &http.Client{
			Timeout: time.Minute * 60,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // Skip verify because the author was lazy
				},
			},
		},
		Server: &http.Server{
			Addr: address,
		},
	}
	cr.Server.Handler = cr
	return
}

func (cr *Cluster)Enable()(bool){
	if cr.socket != nil {
		logDebug("Extara enable")
		return true
	}
	cr.connlock.Lock()
	defer cr.connlock.Unlock()
	if cr.socket != nil {
		logDebug("Extara after lock enable")
		return true
	}
	var (
		err error
		res *http.Response
	)
	logInfo("Enabling cluster")
	wsurl := httpToWs(cr.prefix) +
		fmt.Sprintf("/socket.io/?clusterId=%s&clusterSecret=%s&EIO=4&transport=websocket", cr.username, cr.password)
	logDebug("Websocket url:", wsurl)
	header := http.Header{}
	header.Set("Origin", cr.prefix)

	cr.socket = NewSocket(NewESocket())
	cr.socket.ConnectHandle = func(*Socket){
		cr._enable()
	}
	cr.socket.DisconnectHandle = func(*Socket){
		cr.Disable()
	}
	cr.socket.GetIO().ErrorHandle = func(*ESocket){
		go func(){
			cr.enabled = false
			cr.Disable()()
			if !cr.Enable() {
				logError("Cannot reconnect to server, exit.")
				panic("Cannot reconnect to server, exit.")
			}
		}()
	}
	err = cr.socket.GetIO().Dial(wsurl, header)
	if err != nil {
		logError("Connect websocket error:", err, res)
		return false
	}
	return true
}

func (cr *Cluster)_enable(){
	cr.socket.EmitAck(func(_ uint64, data json.JsonArr){
		data = data.GetArray(0)
		if len(data) < 2 || !data.GetBool(1) {
			logError("Enable failed: " + data.String())
			panic("Enable failed: " + data.String())
		}
		logInfo("get enable ack:", data)
		cr.enabled = true
		cr.keepalive = createInterval(func(){
			cr.KeepAlive()
		}, time.Second * 60)
	}, "enable", json.JsonObj{
		"host": cr.host,
		"port": cr.public_port,
		"version": cr.version,
	})
}

func (cr *Cluster)KeepAlive()(ok bool){
	hits, hbytes := cr.hits, cr.hbytes
	var (
		err error
	)
	err = cr.socket.EmitAck(func(_ uint64, data json.JsonArr){
		data = data.GetArray(0)
		if len(data) > 1 && data.Get(0) == nil {
			logInfo("Keep-alive success:", hits, bytesToUnit((float32)(hbytes)), data)
			cr.hits -= hits
			cr.hbytes -= hbytes
		}else{
			logInfo("Keep-alive failed:", data.Get(0))
			cr.Disable()
			if !cr.Enable() {
				logError("Cannot reconnect to server, exit.")
				panic("Cannot reconnect to server, exit.")
			}
		}
	}, "keep-alive", json.JsonObj{
		"time": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"hits": cr.hits,
		"bytes": cr.hbytes,
	})
	if err != nil {
		logError("Error when keep-alive:", err)
	}
	return err == nil
}

func (cr *Cluster)Disable()(sync func()){
	if !cr.enabled {
		logDebug("Extara disable")
		return func(){}
	}
	logInfo("Disabling cluster")
	if cr.keepalive != nil {
		cr.keepalive()
		cr.keepalive = nil
	}
	if cr.socket != nil {
		sch := make(chan struct{}, 1)
		err := cr.socket.EmitAck(func(_ uint64, data json.JsonArr){
			data = data.GetArray(0)
			logInfo("disable ack:", data)
			if !data.GetBool(1) {
				logError("Disable failed: " + data.String())
				panic("Disable failed: " + data.String())
			}
			sch <- struct{}{}
		}, "disable")
		cr.enabled = false
		cr.socket.Close()
		cr.socket = nil
		if err == nil {
			return func(){
				<- sch
			}
		}
	}
	return func(){}
}

func (cr *Cluster)queryFunc(method string, url string, call func(*http.Request))(res *http.Response, err error){
	var req *http.Request
	req, err = http.NewRequest(method, cr.prefix + url, nil)
	if err != nil { return }
	req.SetBasicAuth(cr.username, cr.password)
	req.Header.Set("User-Agent", cr.useragent)
	if call != nil {
		call(req)
	}
	res, err = cr.client.Do(req)
	return
}

func (cr *Cluster)queryURL(method string, url string)(res *http.Response, err error){
	return cr.queryFunc(method, url, nil)
}

func (cr *Cluster)queryURLHeader(method string, url string, header map[string]string)(res *http.Response, err error){
	return cr.queryFunc(method, url, func(req *http.Request){
		if header != nil {
			for k, v := range header {
				req.Header.Set(k, v)
			}
		}
	})
}

type FileInfo struct{
	Path string `json:"path"`
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

type IFileList struct{
	Files []FileInfo `json:"files"`
}

func (cr *Cluster)getHashPath(hash string)(string){
	return ufile.JoinPath(cr.cachedir, hash[:2], hash)
}

func (cr *Cluster)GetFileList()(files []FileInfo){
	var(
		err error
		res *http.Response
	)
	res, err = cr.queryURL("GET", "/openbmclapi/files")
	if err != nil {
		logError("Query filelist error:", err)
		return nil
	}
	list := new(IFileList)
	err = json.ReadJson(res.Body, &list)
	if err != nil {
		logError("Parse filelist body error:", err)
		return nil
	}
	return list.Files
}

type extFileInfo struct{
	*FileInfo
	dlerr error
	trycount int
}

func (cr *Cluster)SyncFiles(_files []FileInfo, _ctx ...context.Context){
	var ctx context.Context = nil
	if len(_ctx) > 0 {
		ctx = _ctx[0]
	}
	if checkContext(ctx) {
		logWarn("File sync interrupted")
		return
	}
	logInfo("Pre sync files...")
	if cr.issync {
		logWarn("Another sync task is running!")
		return
	}
	cr.issync = true
	defer func(){
		cr.issync = false
	}()
	var files []FileInfo
	if !withContext(ctx, func(){
		files = cr.CheckFiles(_files, make([]FileInfo, 0, 16))
	}) {
		logWarn("File sync interrupted")
		return
	}
	fl := len(files)
	if fl == 0 {
		logInfo("All file was synchronized")
		go cr.gc(_files)
		return
	}
	sort.Slice(files, func(i, j int)(bool){ return files[i].Size > files[j].Size })
	var (
		totalsize float32 = 0
		downloaded float32 = 0
	)
	for i, _ := range files { totalsize += (float32)(files[i].Size) }
	logInfof("Starting sync files, count: %d, total: %s", fl, bytesToUnit(totalsize))
	start := time.Now()
	re := make(chan *extFileInfo, (int)(cr.max_conn))
	fcount := 0
	alive := (uint)(0)
	var (
		dlhandle func(f *extFileInfo, c chan<- *extFileInfo, ctx context.Context)
		handlef func(f *extFileInfo, ctx context.Context)
		dlfile func(f *extFileInfo, ctx context.Context)
	)
	dlhandle = func(f *extFileInfo, c chan<- *extFileInfo, ctx context.Context){
		defer func(){
			alive--
			c <- f
		}()
		var(
			buf []byte
			bi int
			n int
			err error
			res *http.Response
			fd *os.File
		)
		buf, bi = mallocBuf()
		defer func(){
			releaseBuf(bi)
		}()
		p := cr.getHashPath(f.Hash)
		defer func(){
			f.dlerr = err
			if err != nil {
				f.trycount++
				if ufile.IsExist(p) {
					os.Remove(p)
				}
			}
		}()
		logDebug("Downloading:", f.Path)
		for i := 0; i < 3 ;i++ {
			if checkContext(ctx) { return }
			res, err = cr.queryURL("GET", f.Path)
			if err != nil {
				continue
			}
			defer res.Body.Close()
			if checkContext(ctx) { return }
			if res.StatusCode != http.StatusOK {
				err = fmt.Errorf("Unexpected status code: %d", res.StatusCode)
				return
			}
			fd, err = os.Create(p)
			if err != nil {
				continue
			}
			defer fd.Close()
			var t int64 = 0
			for {
				if checkContext(ctx) { return }
				n, err = res.Body.Read(buf)
				if n == 0 {
					if err == io.EOF{
						err = nil
					}
					break
				}
				t += (int64)(n)
				_, err = fd.Write(buf[:n])
				if err != nil { break }
			}
			if t != f.Size && err == nil {
				err = fmt.Errorf("File size wrong %s expect %s", bytesToUnit((float32)(t)), bytesToUnit((float32)(f.Size)))
				if DEBUG {
					f0, _ := os.Open(p)
					b0, _ := io.ReadAll(f0)
					if len(b0) < 16 * 1024 {
						logDebug("File content:", (string)(b0), "//for", f.Path)
					}
				}
			}
			if err != nil {
				fd.Close()
				continue
			}
			break
		}
	}
	handlef = func(f *extFileInfo, ctx context.Context){
		if checkContext(ctx) { return }
		if f.dlerr != nil {
			logErrorf("Download file error: %v %s [%s/%s;%d/%d]%.2f%%", f.dlerr, f.Path,
				bytesToUnit(downloaded), bytesToUnit(totalsize),
				fcount, fl, downloaded / totalsize * 100)
			if f.trycount < 1 { // ps: ????????????????????????????????????, ????????????max_trycount, ???????????????????????????(???:????????????????????????
				c, _ := context.WithCancel(ctx)
				dlfile(f, c)
			}else{
				fcount++
			}
		}else{
			downloaded += (float32)(f.Size)
			fcount++
			logInfof("Downloaded: %s [%s/%s;%d/%d]%.2f%%", f.Path,
				bytesToUnit(downloaded), bytesToUnit(totalsize),
				fcount, fl, downloaded / totalsize * 100)
		}
	}
	dlfile = func(f *extFileInfo, ctx context.Context){
		for alive >= cr.max_conn {
			select{
			case r := <-re:
				c, _ := context.WithCancel(ctx)
				handlef(r, c)
			case <-ctx.Done():
				return
			}
		}
		alive++
		go dlhandle(f, re, ctx)
	}
	for i, _ := range files {
		if checkContext(ctx) {
			logWarn("File sync interrupted")
			return
		}
		c, _ := context.WithCancel(ctx)
		dlfile(&extFileInfo{ FileInfo: &files[i], dlerr: nil, trycount: 0 }, c)
	}
	for fcount < fl {
		select{
		case r := <-re:
			c, _ := context.WithCancel(ctx)
			handlef(r, c)
		case <-ctx.Done():
			logWarn("File sync interrupted")
			return
		}
	}
	use := time.Since(start)
	logInfof("All file was synchronized, use time: %v, %s/s", use, bytesToUnit(totalsize / (float32)(use / time.Second)))
	cr.issync = false
	var flag bool = false
	if use > time.Minute * 10 { // interval time
		logWarn("Synchronization time was more than 10 min, re checking now.")
		_files2 := cr.GetFileList()
		if len(_files2) != len(_files) {
			flag = true
		}else{
			for _, f := range _files2 {
				if checkContext(ctx){
					logWarn("File sync interrupted")
					return
				}
				p := cr.getHashPath(f.Hash)
				if ufile.IsNotExist(p) {
					flag = true
					break
				}
			}
		}
		if flag {
			logWarn("At least one file has changed during file synchronization, re synchronize now.")
			cr.SyncFiles(_files2, ctx)
			return
		}
	}
	go cr.gc(_files)
}

func (cr *Cluster)CheckFiles(files []FileInfo, notf []FileInfo)([]FileInfo){
	logInfo("Starting check files")
	for i, _ := range files {
		if cr.issync && notf == nil {
			logWarn("File check interrupted")
			return nil
		}
		p := cr.getHashPath(files[i].Hash)
		fs, err := os.Stat(p)
		if err == nil {
			if fs.Size() != files[i].Size {
				logInfof("Found wrong size file: '%s'(%s) expect %s",
					p, bytesToUnit((float32)(fs.Size())), bytesToUnit((float32)(files[i].Size)))
				if notf == nil {
					os.Remove(p)
				}else{
					notf = append(notf, files[i])
				}
			}
		}else{
			if notf != nil {
				notf = append(notf, files[i])
			}
			if os.IsNotExist(err) {
				p = ufile.DirPath(p)
				if ufile.IsNotExist(p) {
					os.MkdirAll(p, 0744)
				}
			}else{
				os.Remove(p)
			}
		}
	}
	logInfo("File check finished")
	return notf
}

func (cr *Cluster)gc(files []FileInfo){
	logInfo("Starting global cleanup")
	fileset := make(map[string]struct{})
	for i, _ := range files {
		fileset[cr.getHashPath(files[i].Hash)] = struct{}{}
	}
	stack := make([]string, 0, 1)
	stack = append(stack, cr.cachedir)
	var (
		ok bool
		p string
		n string
		fil []os.DirEntry
		err error
	)
	for len(stack) > 0 {
		p = stack[len(stack) - 1]
		stack = stack[:len(stack) - 1]
		fil, err = os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, f := range fil {
			if cr.issync {
				logWarn("Global cleanup interrupted")
				return
			}
			n = ufile.JoinPath(p, f.Name())
			if ufile.IsDir(n) {
				stack = append(stack, n)
			}else if _, ok = fileset[n]; !ok {
				logInfo("Found outdated file:", n)
				os.Remove(n)
			}
		}
	}
	logInfo("Global cleanup finished")
}

func (cr *Cluster)DownloadFile(hash string)(bool){
	var(
		err error
		res *http.Response
		fd *os.File
	)
	res, err = cr.queryURL("GET", "/openbmclapi/download/" + hash + "?noopen=1")
	if err != nil {
		logError("Query file error:", err)
		return false
	}
	fd, err = os.Create(cr.getHashPath(hash))
	if err != nil {
		logError("Create file error:", err)
		return false
	}
	defer fd.Close()
	_, err = io.Copy(fd, res.Body)
	if err != nil {
		logError("Write file error:", err)
		return false
	}
	return true
}

func (cr *Cluster)ServeHTTP(response http.ResponseWriter, request *http.Request){
	method := request.Method
	url := request.URL
	rawpath := url.EscapedPath()
	if SHOW_SERVE_INFO {
		go logInfo("serve url:", url.String())
	}
	switch{
	case strings.HasPrefix(rawpath, "/download/"):
		if method == "GET" {
			path := cr.getHashPath(rawpath[10:])
			if ufile.IsNotExist(path) {
				if !cr.DownloadFile(path) {
					response.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
			if name := request.Form.Get("name"); name != "" {
				response.Header().Set("Content-Disposition", "attachment; filename=" + name)
			}
			response.Header().Set("Cache-Control", "max-age=2592000") // 30 days
			fd, err := os.Open(path)
			if err != nil {
				response.WriteHeader(http.StatusInternalServerError)
				return
			}
			response.WriteHeader(http.StatusOK)
			buf, bi := mallocBuf()
			defer func(){
				releaseBuf(bi)
			}()
			var (
				hb uint64
				n int
			)
			for {
				n, err = fd.Read(buf)
				if err != nil {
					if err == io.EOF {
						err = nil
						break
					}
					logError("Error when serving download read file:", err)
					return
				}
				if n == 0 {
					break
				}
				_, err = response.Write(buf[:n])
				if err != nil {
					if !IGNORE_SERVE_ERROR {
						logError("Error when serving download:", err)
					}
					return
				}
				hb += (uint64)(n)
			}
			cr.hits++
			cr.hbytes += hb
			return
		}
	case strings.HasPrefix(rawpath, "/measure/"):
		if method == "GET"{
			if request.Header.Get("x-openbmclapi-secret") != cr.password {
				response.WriteHeader(http.StatusForbidden)
				return
			}
			n, e := strconv.Atoi(rawpath[9:])
			if e != nil || n < 0 || n > 200 {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			response.WriteHeader(http.StatusOK)
			response.Write(make([]byte, n * 1024 * 1024))
			return
		}
	}
	response.WriteHeader(http.StatusNotFound)
}

