
package main

import (
	os "os"
	io "io"
	ioutil "io/ioutil"
	time "time"
	sort "sort"
	strings "strings"
	strconv "strconv"
	fmt "fmt"
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

	enabled bool
	socket *Socket
	keepalive func()(bool)

	client *http.Client
	Server *http.Server
}

func newCluster(
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

		enabled: false,
		socket: nil,

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
	if cr.enabled {
		return true
	}
	var (
		err error
		res *http.Response
	)
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
		cr.Disable()
		cr.Enable()
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
		logDebug("get enable ack:", data)
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
		logDebug("get keep-alive ack:", data)
		logInfo("Keep-alive success:", hits, bytesToUnit((float32)(hbytes)))
		cr.hits -= hits
		cr.hbytes -= hbytes
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

func (cr *Cluster)Disable(){
	logInfo("Disabling cluster")
	if cr.keepalive != nil {
		cr.keepalive()
		cr.keepalive = nil
	}
	if cr.enabled {
		cr.socket.EmitAck(func(_ uint64, data json.JsonArr){
			logDebug("disable ack:", data)
			cr.enabled = false
			cr.socket.Close()
			cr.socket = nil
		}, "disable")
	}
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
	Err error
}

func (cr *Cluster)SyncFiles(_files []FileInfo){
	logInfo("Pre sync files...")
	files := make([]FileInfo, 0, len(_files) / 3)
	for _, f := range _files {
		p := cr.getHashPath(f.Hash)
		if ufile.IsNotExist(p) {
			files = append(files, f)
			p = ufile.DirPath(p)
			if ufile.IsNotExist(p) {
				os.MkdirAll(p, 0744)
			}
		}
	}
	fl := len(files)
	if fl == 0 {
		logInfo("All file was synchronized")
		go cr.gc(_files)
		return
	}
	sort.Slice(files, func(i, j int)(bool){ return files[i].Hash < files[j].Hash })
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
		dlhandle func(f *extFileInfo, c chan<- *extFileInfo)
		handlef func(f *extFileInfo)
		dlfile func(f *FileInfo)
	)
	dling := 0
	dlhandle = func(f *extFileInfo, c chan<- *extFileInfo){
		defer func(){
			alive--
			c <- f
		}()
		var(
			buf []byte = make([]byte, 1024 * 1024 * 8) // 8MB
			n int
			err error
			res *http.Response
			fd *os.File
		)
		p := cr.getHashPath(f.Hash)
		defer func(){
			if err != nil {
				if ufile.IsExist(p) {
					ufile.RemoveFile(p)
				}
			}
		}()
		logDebug("Downloading:", f.Path)
		for i := 0; i < 3 ;i++ {
			res, err = cr.queryURL("GET", f.Path)
			if err != nil {
				continue
			}
			defer res.Body.Close()
			fd, err = os.Create(p)
			if err != nil {
				continue
			}
			defer fd.Close()
			for {
				n, err = res.Body.Read(buf)
				if n == 0 && (err == nil || err == io.EOF) {
					err = nil
					break
				}
				_, err = fd.Write(buf[:n])
				if err != nil { break }
				dling += n
			}
			if err != nil {
				fd.Close()
				continue
			}
			return
		}
		if err != nil {
			f.Err = err
		}
	}
	lastt := time.Now()
	handlef = func(f *extFileInfo){
		if f.Err != nil {
			logError("Download file error:", f.Path, f.Err)
			dlfile(f.FileInfo)
		}else{
			d := (float32)(dling)
			dling = 0
			fcount++
			downloaded += (float32)(f.Size)
			logInfof("Downloaded: %s [%s/%s:%s/s]%.2f%%", f.Path,
					bytesToUnit(downloaded), bytesToUnit(totalsize), 
					bytesToUnit(d / (float32)(time.Since(lastt)) * (float32)(time.Second)), downloaded / totalsize * 100)
			lastt = time.Now()
		}
	}
	dlfile = func(f *FileInfo){
		for alive >= cr.max_conn {
			select{
			case r := <-re:
				handlef(r)
			}
		}
		alive++
		go dlhandle(&extFileInfo{ FileInfo: f, Err: nil }, re)
	}
	for i, _ := range files {
		dlfile(&files[i])
	}
	for fcount < fl {
		handlef(<-re)
	}
	use := time.Since(start)
	logInfof("All file was synchronized, use time: %v, %s/s", use, bytesToUnit(totalsize / (float32)(use / time.Second)))
	var flag bool = false
	if use > time.Minute * 10 { // interval time
		logWarn("Synchronization time was more than 10 min, re checking now.")
		_files2 := cr.GetFileList()
		if len(_files2) != len(_files) {
			flag = true
		}else{
			for _, f := range _files2 {
				p := cr.getHashPath(f.Hash)
				if ufile.IsNotExist(p) {
					flag = true
					break
				}
			}
		}
		if flag {
			logWarn("At least one file has changed during file synchronization, re synchronize now.")
			cr.SyncFiles(_files2)
			return
		}
	}
	go cr.gc(_files)
}

func (cr *Cluster)gc(files []FileInfo){
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
		fil []os.FileInfo
		err error
	)
	for len(stack) > 0 {
		p = stack[len(stack) - 1]
		stack = stack[:len(stack) - 1]
		fil, err = ioutil.ReadDir(p)
		if err != nil {
			continue
		}
		for _, f := range fil {
			n = ufile.JoinPath(p, f.Name())
			if _, ok = fileset[n]; !ok {
				ufile.RemoveFile(n)
			}
		}
	}
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
	logInfo("serve url:", url.String())
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
			buf := make([]byte, 1024 * 1024 * 8) // chunk size = 8MB
			var (
				hb uint64
				n int
			)
			for {
				n, err = fd.Read(buf)
				if err != nil {
					if err == io.EOF { break }
					logError("Error when serving download read file:", err)
					return
				}
				if n == 0 {
					break
				}
				_, err = response.Write(buf[:n])
				if err != nil {
					logError("Error when serving download:", err)
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

