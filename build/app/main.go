//go:build windows

package main

import (
    "context"
    "crypto/sha256"
    "embed"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "io"
    "net"
    "net/http"
    "net/url"
    "os"
    "os/exec"
    "path/filepath"
    "strconv"
    "strings"
    "sync"
    "syscall"
    "time"
    "unsafe"
)

const (
    coreVersion     = "1.5"
    embeddedVersion = "1.5"
    manifestURL     = "https://raw.githubusercontent.com/servicegg/English1000-Updates/main/version.json"
    remoteAppURL    = "https://raw.githubusercontent.com/servicegg/English1000-Updates/main/app.html"
    maxHTMLSize     = 2 << 20
    maxEXESize      = 25 << 20
    controlAddr     = "127.0.0.1:18473"
    pollInterval    = 1 * time.Second
)

//go:embed app.html updater.exe
var content embed.FS

type manifest struct {
    Version    string `json:"version"`
    SHA256     string `json:"sha256"`
    EXEVersion string `json:"exe_version"`
    EXESHA256  string `json:"exe_sha256"`
    EXEURL     string `json:"exe_url"`
}

type eventHub struct {
    mu sync.Mutex
    subs map[chan string]struct{}
}

func newEventHub() *eventHub { return &eventHub{subs: make(map[chan string]struct{})} }
func (h *eventHub) add(ch chan string) { h.mu.Lock(); h.subs[ch]=struct{}{}; h.mu.Unlock() }
func (h *eventHub) remove(ch chan string) { h.mu.Lock(); delete(h.subs,ch); h.mu.Unlock() }
func (h *eventHub) broadcast(version string) {
    h.mu.Lock()
    defer h.mu.Unlock()
    payload := fmt.Sprintf("{\"version\":%q}", version)
    for ch := range h.subs {
        select { case ch <- payload: default: }
    }
}

type updateStatus struct {
    State          string `json:"state"`
    Message        string `json:"message"`
    Progress       int    `json:"progress"`
    CurrentVersion string `json:"current_version"`
    LatestVersion  string `json:"latest_version"`
    HasUpdate      bool   `json:"has_update"`
    LastChecked    string `json:"last_checked"`
}

type updateMonitor struct {
    mu sync.RWMutex
    s  updateStatus
}

func newUpdateMonitor(current string) *updateMonitor {
    return &updateMonitor{s:updateStatus{
        State:"checking", Message:"Проверяю обновления…", Progress:0,
        CurrentVersion:current, LastChecked:time.Now().Format(time.RFC3339),
    }}
}

func (m *updateMonitor) set(state,message,current,latest string,progress int,hasUpdate bool) {
    if m==nil { return }
    m.mu.Lock()
    defer m.mu.Unlock()
    if current!="" { m.s.CurrentVersion=current }
    if latest!="" { m.s.LatestVersion=latest }
    m.s.State=state
    m.s.Message=message
    m.s.Progress=progress
    m.s.HasUpdate=hasUpdate
    m.s.LastChecked=time.Now().Format(time.RFC3339)
}

func (m *updateMonitor) snapshot() updateStatus {
    if m==nil { return updateStatus{State:"offline",Message:"Статус обновлений недоступен"} }
    m.mu.RLock()
    defer m.mu.RUnlock()
    return m.s
}

type progressReader struct {
    r io.Reader
    total int64
    size int64
    last int
    cb func(int)
}

func (p *progressReader) Read(buf []byte)(int,error){
    n,err:=p.r.Read(buf)
    if n>0 {
        p.total+=int64(n)
        if p.cb!=nil {
            pct:=-1
            if p.size>0 {
                pct=int((p.total*100)/p.size)
                if pct>100 { pct=100 }
            }
            if pct!=p.last {
                p.last=pct
                p.cb(pct)
            }
        }
    }
    return n,err
}

func messageBox(title, body string) {
    user32 := syscall.NewLazyDLL("user32.dll")
    proc := user32.NewProc("MessageBoxW")
    t,_ := syscall.UTF16PtrFromString(body)
    c,_ := syscall.UTF16PtrFromString(title)
    proc.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x10)
}

func minimizeWindowForPID(pid int) {
    if pid <= 0 { return }
    user32 := syscall.NewLazyDLL("user32.dll")
    enumWindows := user32.NewProc("EnumWindows")
    getWindowThreadProcessId := user32.NewProc("GetWindowThreadProcessId")
    isWindowVisible := user32.NewProc("IsWindowVisible")
    showWindow := user32.NewProc("ShowWindow")
    cb := syscall.NewCallback(func(hwnd uintptr, lparam uintptr) uintptr {
        var windowPID uint32
        getWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&windowPID)))
        if int(windowPID) == pid {
            visible, _, _ := isWindowVisible.Call(hwnd)
            if visible != 0 {
                showWindow.Call(hwnd, 6)
                return 0
            }
        }
        return 1
    })
    enumWindows.Call(cb, 0)
}

func findEnglishWindow(pid int) uintptr {
    user32 := syscall.NewLazyDLL("user32.dll")
    enumWindows := user32.NewProc("EnumWindows")
    getWindowThreadProcessId := user32.NewProc("GetWindowThreadProcessId")
    isWindowVisible := user32.NewProc("IsWindowVisible")
    getWindowTextLength := user32.NewProc("GetWindowTextLengthW")
    getWindowText := user32.NewProc("GetWindowTextW")
    var byTitle uintptr
    var byPID uintptr
    cb := syscall.NewCallback(func(hwnd uintptr, lparam uintptr) uintptr {
        visible,_,_:=isWindowVisible.Call(hwnd)
        if visible==0 { return 1 }
        var windowPID uint32
        getWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&windowPID)))
        if int(windowPID)==pid && byPID==0 { byPID=hwnd }
        n,_,_:=getWindowTextLength.Call(hwnd)
        if n>0 {
            buf:=make([]uint16,int(n)+1)
            getWindowText.Call(hwnd,uintptr(unsafe.Pointer(&buf[0])),n+1)
            title:=strings.ToLower(syscall.UTF16ToString(buf))
            if strings.Contains(title,"english1000") || strings.Contains(title,"english 1000") {
                byTitle=hwnd
                return 0
            }
        }
        return 1
    })
    enumWindows.Call(cb,0)
    if byTitle!=0 { return byTitle }
    return byPID
}

func makeWindowFramelessForPID(pid int) bool {
    hwnd:=findEnglishWindow(pid)
    if hwnd==0 { return false }
    user32:=syscall.NewLazyDLL("user32.dll")
    getWindowLongPtr:=user32.NewProc("GetWindowLongPtrW")
    setWindowLongPtr:=user32.NewProc("SetWindowLongPtrW")
    setWindowPos:=user32.NewProc("SetWindowPos")
    const GWL_STYLE = ^uintptr(15)
    const WS_CAPTION = uintptr(0x00C00000)
    const WS_SYSMENU = uintptr(0x00080000)
    const SWP_NOSIZE = uintptr(0x0001)
    const SWP_NOMOVE = uintptr(0x0002)
    const SWP_NOZORDER = uintptr(0x0004)
    const SWP_NOACTIVATE = uintptr(0x0010)
    const SWP_FRAMECHANGED = uintptr(0x0020)
    style,_,_:=getWindowLongPtr.Call(hwnd,GWL_STYLE)
    newStyle:=style &^ WS_CAPTION &^ WS_SYSMENU
    setWindowLongPtr.Call(hwnd,GWL_STYLE,newStyle)
    setWindowPos.Call(hwnd,0,0,0,0,SWP_NOSIZE|SWP_NOMOVE|SWP_NOZORDER|SWP_NOACTIVATE|SWP_FRAMECHANGED)
    return true
}

func dragWindowForPID(pid int) {
    hwnd:=findEnglishWindow(pid)
    if hwnd==0 { return }
    user32:=syscall.NewLazyDLL("user32.dll")
    releaseCapture:=user32.NewProc("ReleaseCapture")
    sendMessage:=user32.NewProc("SendMessageW")
    const WM_NCLBUTTONDOWN = uintptr(0x00A1)
    const HTCAPTION = uintptr(2)
    releaseCapture.Call()
    sendMessage.Call(hwnd,WM_NCLBUTTONDOWN,HTCAPTION,0)
}

func findEdge() string {
    var candidates []string
    if p:=os.Getenv("ProgramFiles(x86)"); p!="" { candidates=append(candidates,filepath.Join(p,"Microsoft","Edge","Application","msedge.exe")) }
    if p:=os.Getenv("ProgramFiles"); p!="" { candidates=append(candidates,filepath.Join(p,"Microsoft","Edge","Application","msedge.exe")) }
    if p:=os.Getenv("LOCALAPPDATA"); p!="" { candidates=append(candidates,filepath.Join(p,"Microsoft","Edge","Application","msedge.exe")) }
    if p,err:=exec.LookPath("msedge.exe"); err==nil { candidates=append(candidates,p) }
    for _,p:=range candidates { if st,err:=os.Stat(p); err==nil && !st.IsDir() { return p } }
    return ""
}

func logLine(appDir, s string) {
    f,err:=os.OpenFile(filepath.Join(appDir,"update.log"),os.O_CREATE|os.O_APPEND|os.O_WRONLY,0644)
    if err!=nil { return }
    defer f.Close()
    fmt.Fprintf(f,"%s | %s\r\n",time.Now().Format("2006-01-02 15:04:05.000"),s)
}

func versionParts(v string) []int {
    v=strings.TrimSpace(strings.TrimPrefix(strings.ToLower(v),"v"))
    bits:=strings.Split(v,".")
    out:=make([]int,0,len(bits))
    for _,b:=range bits { n,err:=strconv.Atoi(b); if err!=nil{return nil}; out=append(out,n) }
    return out
}

func compareVersions(a,b string) int {
    aa,bb:=versionParts(a),versionParts(b)
    if aa==nil || bb==nil { return strings.Compare(a,b) }
    n:=len(aa); if len(bb)>n { n=len(bb) }
    for i:=0;i<n;i++ {
        av,bv:=0,0
        if i<len(aa){av=aa[i]}
        if i<len(bb){bv=bb[i]}
        if av<bv{return -1}; if av>bv{return 1}
    }
    return 0
}

func atomicWrite(path string,data []byte) error {
    tmp:=path+".tmp"
    if err:=os.WriteFile(tmp,data,0644);err!=nil{return err}
    _=os.Remove(path)
    return os.Rename(tmp,path)
}

func readInstalledVersion(path string) string {
    b,err:=os.ReadFile(path); if err!=nil{return "0"}
    return strings.TrimSpace(string(b))
}

func ensureEmbeddedBaseline(appDir,htmlPath,versionPath string) string {
    installed:=readInstalledVersion(versionPath)
    if _,err:=os.Stat(htmlPath);err!=nil || compareVersions(installed,embeddedVersion)<0 {
        html,err:=content.ReadFile("app.html")
        if err==nil {
            if err=atomicWrite(htmlPath,html);err==nil {
                _=os.WriteFile(versionPath,[]byte(embeddedVersion),0644)
                installed=embeddedVersion
                logLine(appDir,"installed embedded baseline v"+embeddedVersion)
            }
        }
    }
    if installed=="0"{installed=embeddedVersion}
    return installed
}

func httpClient() *http.Client {
    tr:=&http.Transport{
        Proxy:http.ProxyFromEnvironment,
        DialContext:(&net.Dialer{Timeout:2500*time.Millisecond,KeepAlive:30*time.Second}).DialContext,
        ForceAttemptHTTP2:true,
        MaxIdleConns:4,
        IdleConnTimeout:30*time.Second,
        TLSHandshakeTimeout:2500*time.Millisecond,
        ResponseHeaderTimeout:2500*time.Millisecond,
    }
    return &http.Client{Timeout:8*time.Second,Transport:tr}
}

func getBytesProgress(client *http.Client,address string,limit int64,cb func(int))([]byte,error){
    req,err:=http.NewRequest(http.MethodGet,address,nil);if err!=nil{return nil,err}
    req.Header.Set("User-Agent","English1000-SelfUpdater/1.5")
    req.Header.Set("Cache-Control","no-cache, no-store, must-revalidate")
    req.Header.Set("Pragma","no-cache")
    resp,err:=client.Do(req);if err!=nil{return nil,err}
    defer resp.Body.Close()
    if resp.StatusCode!=http.StatusOK{return nil,fmt.Errorf("HTTP %d",resp.StatusCode)}
    if cb!=nil { cb(0) }
    limited:=io.LimitReader(resp.Body,limit+1)
    pr:=&progressReader{r:limited,size:resp.ContentLength,last:-2,cb:cb}
    b,err:=io.ReadAll(pr);if err!=nil{return nil,err}
    if int64(len(b))>limit{return nil,fmt.Errorf("file too large")}
    if cb!=nil { cb(100) }
    return b,nil
}

func getBytes(client *http.Client,address string,limit int64)([]byte,error){
    return getBytesProgress(client,address,limit,nil)
}

func fetchManifest(client *http.Client)(manifest,error){
    stamp:=strconv.FormatInt(time.Now().UnixNano(),10)
    b,err:=getBytes(client,manifestURL+"?t="+stamp,64<<10);if err!=nil{return manifest{},err}
    var m manifest
    if err:=json.Unmarshal(b,&m);err!=nil{return manifest{},err}
    return m,nil
}

func localFileSHA256(path string) string {
    b,err:=os.ReadFile(path);if err!=nil{return ""}
    sum:=sha256.Sum256(b)
    return hex.EncodeToString(sum[:])
}

func updateHTML(client *http.Client,appDir,htmlPath,versionPath,installed string,m manifest,monitor *updateMonitor)(string,bool){
    if versionParts(m.Version)==nil{return installed,false}
    expected:=strings.ToLower(strings.TrimSpace(m.SHA256))
    if len(expected)!=64{logLine(appDir,"html update rejected: invalid sha256");return installed,false}

    localHash:=strings.ToLower(localFileSHA256(htmlPath))
    versionCmp:=compareVersions(m.Version,installed)
    needsUpdate:=versionCmp>0 || localHash=="" || localHash!=expected
    if !needsUpdate{return installed,false}

    reason:="new version"
    if versionCmp<=0 && localHash!=expected { reason="hash repair" }
    logLine(appDir,"html refresh required: "+reason+" local="+localHash+" expected="+expected)
    monitor.set("available","Есть обновление",installed,m.Version,0,true)

    stamp:=strconv.FormatInt(time.Now().UnixNano(),10)
    monitor.set("downloading","Скачивание интерфейса",installed,m.Version,0,true)
    html,err:=getBytesProgress(client,remoteAppURL+"?v="+url.QueryEscape(m.Version)+"&t="+stamp,maxHTMLSize,func(p int){
        monitor.set("downloading","Скачивание интерфейса",installed,m.Version,p,true)
    })
    if err!=nil{logLine(appDir,"html update download failed: "+err.Error());monitor.set("error","Ошибка загрузки обновления",installed,m.Version,0,true);return installed,false}
    sum:=sha256.Sum256(html);got:=hex.EncodeToString(sum[:])
    if !strings.EqualFold(got,expected){logLine(appDir,"html update rejected: sha256 mismatch");monitor.set("error","Проверка обновления не пройдена",installed,m.Version,0,true);return installed,false}
    monitor.set("installing","Установка интерфейса",installed,m.Version,100,true)
    if err:=atomicWrite(htmlPath,html);err!=nil{logLine(appDir,"html update install failed: "+err.Error());monitor.set("error","Ошибка установки обновления",installed,m.Version,100,true);return installed,false}
    installed=strings.TrimSpace(m.Version)
    _=os.WriteFile(versionPath,[]byte(installed),0644)
    logLine(appDir,"updated/repaired HTML to v"+installed)
    monitor.set("up_to_date","Обновлений нет",installed,m.Version,100,false)
    return installed,true
}

func ensureUpdater(appDir string)(string,error){
    b,err:=content.ReadFile("updater.exe");if err!=nil{return "",err}
    dir:=filepath.Join(appDir,"Updater");if err:=os.MkdirAll(dir,0755);err!=nil{return "",err}
    path:=filepath.Join(dir,"English1000Updater.exe")
    sum:=sha256.Sum256(b);expected:=hex.EncodeToString(sum[:])
    current,err:=os.ReadFile(path)
    if err==nil { s:=sha256.Sum256(current); if strings.EqualFold(hex.EncodeToString(s[:]),expected){return path,nil} }
    monitor.set("installing","Подготовка установки",current,m.EXEVersion,100,true)
    if err:=atomicWrite(path,b);err!=nil{monitor.set("error","Ошибка подготовки обновления",current,m.EXEVersion,100,true);return "",err}
    return path,nil
}

func stageCoreUpdate(client *http.Client,appDir string,m manifest,monitor *updateMonitor,current string)(string,error){
    if m.EXEURL=="" || len(strings.TrimSpace(m.EXESHA256))!=64{return "",fmt.Errorf("invalid exe manifest")}
    stamp:=strconv.FormatInt(time.Now().UnixNano(),10)
    sep:="?";if strings.Contains(m.EXEURL,"?"){sep="&"}
    monitor.set("available","Есть обновление",current,m.EXEVersion,0,true)
    monitor.set("downloading","Скачивание приложения",current,m.EXEVersion,0,true)
    b,err:=getBytesProgress(client,m.EXEURL+sep+"v="+url.QueryEscape(m.EXEVersion)+"&t="+stamp,maxEXESize,func(p int){
        monitor.set("downloading","Скачивание приложения",current,m.EXEVersion,p,true)
    });if err!=nil{monitor.set("error","Ошибка загрузки обновления",current,m.EXEVersion,0,true);return "",err}
    sum:=sha256.Sum256(b);got:=hex.EncodeToString(sum[:])
    if !strings.EqualFold(got,strings.TrimSpace(m.EXESHA256)){monitor.set("error","Проверка обновления не пройдена",current,m.EXEVersion,0,true);return "",fmt.Errorf("sha256 mismatch")}
    dir:=filepath.Join(appDir,"Updates");if err:=os.MkdirAll(dir,0755);err!=nil{return "",err}
    path:=filepath.Join(dir,"English1000_"+strings.ReplaceAll(m.EXEVersion,".","_")+".exe")
    if err:=atomicWrite(path,b);err!=nil{return "",err}
    return path,nil
}

func launchCoreUpdater(appDir string,m manifest,staged string) error {
    updater,err:=ensureUpdater(appDir);if err!=nil{return err}
    target,err:=os.Executable();if err!=nil{return err}
    target,_=filepath.Abs(target)
    logPath:=filepath.Join(appDir,"update.log")
    args:=[]string{
        "--source="+staged,
        "--target="+target,
        "--sha256="+strings.ToLower(strings.TrimSpace(m.EXESHA256)),
        "--pid="+strconv.Itoa(os.Getpid()),
        "--log="+logPath,
    }
    cmd:=exec.Command(updater,args...)
    cmd.SysProcAttr=&syscall.SysProcAttr{HideWindow:true}
    return cmd.Start()
}

func startControlServer(hub *eventHub,monitor *updateMonitor,closeCh chan struct{},minimizeCh chan struct{},dragCh chan struct{})(*http.Server,error){
    mux:=http.NewServeMux()
    mux.HandleFunc("/events",func(w http.ResponseWriter,r *http.Request){
        w.Header().Set("Access-Control-Allow-Origin","*")
        w.Header().Set("Access-Control-Allow-Private-Network","true")
        w.Header().Set("Cache-Control","no-cache")
        w.Header().Set("Content-Type","text/event-stream")
        w.Header().Set("Connection","keep-alive")
        flusher,ok:=w.(http.Flusher);if !ok{http.Error(w,"stream unsupported",500);return}
        ch:=make(chan string,2);hub.add(ch);defer hub.remove(ch)
        fmt.Fprint(w,"event: hello\ndata: {}\n\n");flusher.Flush()
        keep:=time.NewTicker(15*time.Second);defer keep.Stop()
        for {
            select {
            case payload:=<-ch:
                fmt.Fprintf(w,"event: update\ndata: %s\n\n",payload);flusher.Flush()
            case <-keep.C:
                fmt.Fprint(w,": ping\n\n");flusher.Flush()
            case <-r.Context().Done():
                return
            }
        }
    })
    mux.HandleFunc("/status",func(w http.ResponseWriter,r *http.Request){
        w.Header().Set("Access-Control-Allow-Origin","*")
        w.Header().Set("Access-Control-Allow-Private-Network","true")
        w.Header().Set("Access-Control-Allow-Methods","GET, OPTIONS")
        w.Header().Set("Cache-Control","no-store")
        if r.Method==http.MethodOptions { w.WriteHeader(http.StatusNoContent); return }
        w.Header().Set("Content-Type","application/json; charset=utf-8")
        _=json.NewEncoder(w).Encode(monitor.snapshot())
    })
    mux.HandleFunc("/close",func(w http.ResponseWriter,r *http.Request){
        w.Header().Set("Access-Control-Allow-Origin","*")
        w.Header().Set("Access-Control-Allow-Private-Network","true")
        w.WriteHeader(http.StatusNoContent)
        select{case closeCh<-struct{}{}:default:}
    })
    mux.HandleFunc("/minimize",func(w http.ResponseWriter,r *http.Request){
        w.Header().Set("Access-Control-Allow-Origin","*")
        w.Header().Set("Access-Control-Allow-Private-Network","true")
        w.WriteHeader(http.StatusNoContent)
        select{case minimizeCh<-struct{}{}:default:}
    })
    mux.HandleFunc("/drag",func(w http.ResponseWriter,r *http.Request){
        w.Header().Set("Access-Control-Allow-Origin","*")
        w.Header().Set("Access-Control-Allow-Private-Network","true")
        w.WriteHeader(http.StatusNoContent)
        select{case dragCh<-struct{}{}:default:}
    })
    srv:=&http.Server{Addr:controlAddr,Handler:mux,ReadHeaderTimeout:2*time.Second}
    ln,err:=net.Listen("tcp",controlAddr);if err!=nil{return nil,err}
    go func(){_=srv.Serve(ln)}()
    return srv,nil
}

func main(){
    local:=os.Getenv("LOCALAPPDATA");if local==""{local=os.TempDir()}
    appDir:=filepath.Join(local,"English1000")
    if err:=os.MkdirAll(appDir,0755);err!=nil{messageBox("English 1000","Не удалось создать папку приложения.");return}

    htmlPath:=filepath.Join(appDir,"app.html")
    versionPath:=filepath.Join(appDir,"app.version")
    installed:=ensureEmbeddedBaseline(appDir,htmlPath,versionPath)
    client:=httpClient()
    monitor:=newUpdateMonitor(installed)

    if m,err:=fetchManifest(client);err==nil{
        monitor.set("checking","Проверяю обновления…",installed,m.Version,0,false)
        if versionParts(m.EXEVersion)!=nil && compareVersions(m.EXEVersion,coreVersion)>0{
            staged,e:=stageCoreUpdate(client,appDir,m,monitor,installed)
            if e==nil{e=launchCoreUpdater(appDir,m,staged)}
            if e==nil{logLine(appDir,"staged core v"+m.EXEVersion+"; restarting");return}
            logLine(appDir,"core update failed: "+e.Error())
        }
        var changed bool
        installed,changed=updateHTML(client,appDir,htmlPath,versionPath,installed,m,monitor)
        _=changed
        if compareVersions(m.Version,installed)<=0 && compareVersions(m.EXEVersion,coreVersion)<=0 {
            monitor.set("up_to_date","Обновлений нет",installed,m.Version,100,false)
        }
    } else {
        monitor.set("offline","Не удалось проверить",installed,"",0,false)
    }

    closeCh:=make(chan struct{},2)
    minimizeCh:=make(chan struct{},2)
    dragCh:=make(chan struct{},2)
    hub:=newEventHub()
    srv,err:=startControlServer(hub,monitor,closeCh,minimizeCh,dragCh)
    if err!=nil{messageBox("English 1000","Не удалось запустить модуль мгновенных обновлений. Закрой другие копии English 1000 и попробуй снова.");return}
    defer func(){
        ctx,cancel:=context.WithTimeout(context.Background(),500*time.Millisecond)
        defer cancel()
        _=srv.Shutdown(ctx)
    }()

    edge:=findEdge()
    if edge==""{messageBox("English 1000","Microsoft Edge не найден. В Windows 10/11 он обычно установлен по умолчанию.");return}
    abs,err:=filepath.Abs(htmlPath);if err!=nil{abs=htmlPath}
    u:=url.URL{Scheme:"file",Path:filepath.ToSlash(abs)}
    q:=u.Query()
    q.Set("v",installed)
    q.Set("t",strconv.FormatInt(time.Now().UnixNano(),10))
    u.RawQuery=q.Encode()
    profileDir:=filepath.Join(appDir,"Profile")
    cmd:=exec.Command(edge,"--app="+u.String(),"--user-data-dir="+profileDir,"--no-first-run","--disable-features=msEdgeSidebarV2")
    cmd.SysProcAttr=&syscall.SysProcAttr{HideWindow:true}
    if err:=cmd.Start();err!=nil{messageBox("English 1000",fmt.Sprintf("Не удалось открыть приложение:\n%v",err));return}
    go func(){
        deadline:=time.Now().Add(5*time.Second)
        for time.Now().Before(deadline) {
            if makeWindowFramelessForPID(cmd.Process.Pid) {
                logLine(appDir,"frameless window enabled")
                return
            }
            time.Sleep(120*time.Millisecond)
        }
        logLine(appDir,"frameless window not found; using normal frame")
    }()
    go func(){_=cmd.Wait();select{case closeCh<-struct{}{}:default:}}()

    ticker:=time.NewTicker(pollInterval)
    defer ticker.Stop()
    for{
        select{
        case <-ticker.C:
            monitor.set("checking","Проверяю обновления…",installed,"",0,false)
            m,err:=fetchManifest(client);if err!=nil{monitor.set("offline","Не удалось проверить",installed,"",0,false);continue}
            if versionParts(m.EXEVersion)!=nil && compareVersions(m.EXEVersion,coreVersion)>0{
                staged,e:=stageCoreUpdate(client,appDir,m,monitor,installed)
                if e==nil{e=launchCoreUpdater(appDir,m,staged)}
                if e==nil{
                    monitor.set("installing","Установка приложения",installed,m.EXEVersion,100,true)
                    logLine(appDir,"live core update to v"+m.EXEVersion+" triggered")
                    _=cmd.Process.Kill()
                    time.Sleep(120*time.Millisecond)
                    return
                }
                logLine(appDir,"live core update failed: "+e.Error())
            }
            next,changed:=updateHTML(client,appDir,htmlPath,versionPath,installed,m,monitor)
            installed=next
            if changed{hub.broadcast(installed)}
            if !changed && compareVersions(m.Version,installed)<=0 && compareVersions(m.EXEVersion,coreVersion)<=0 {
                monitor.set("up_to_date","Обновлений нет",installed,m.Version,100,false)
            }
        case <-minimizeCh:
            minimizeWindowForPID(cmd.Process.Pid)
        case <-dragCh:
            dragWindowForPID(cmd.Process.Pid)
        case <-closeCh:
            return
        }
    }
}
