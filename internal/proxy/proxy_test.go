package proxy

import (
 "errors"
 "net/http"
 "net/http/httptest"
 "os"
 "path/filepath"
 "strings"
 "testing"
)

func TestVerifyFiles(t *testing.T) {
 t.Parallel()
 root:=t.TempDir()
 if err:=os.Mkdir(filepath.Join(root,"dir"),0700); err!=nil { t.Fatal(err) }
 if err:=os.WriteFile(filepath.Join(root,"dir","file"),[]byte("abc"),0600); err!=nil { t.Fatal(err) }
 if err:=os.Symlink("dir",filepath.Join(root,"link")); err!=nil { t.Fatal(err) }
 h:=handler{cfg:Config{PhysicalData:root}}
 tests:=[]struct{name string; files []file; bad bool}{
  {"complete",[]file{{Name:"dir/file",Size:new(int64(3)),Progress:1}},false},
  {"missing",[]file{{Name:"dir/missing",Size:new(int64(3)),Progress:1}},true},
  {"wrong size",[]file{{Name:"dir/file",Size:new(int64(2)),Progress:1}},true},
  {"partial",[]file{{Name:"dir/file",Size:new(int64(3)),Progress:0.5}},true},
  {"parent traversal",[]file{{Name:"../file",Size:new(int64(3)),Progress:1}},true},
  {"symlink",[]file{{Name:"link/file",Size:new(int64(3)),Progress:1}},true},
  {"empty list",nil,true},
 }
 for _,tt:=range tests { t.Run(tt.name,func(t *testing.T){t.Parallel(); err:=h.verifyFiles(root,tt.files,func(string)error{return nil}); if (err!=nil)!=tt.bad {t.Fatalf("verifyFiles error = %v, want error %v",err,tt.bad)} }) }
 if err:=h.verifyFiles(root,[]file{{Name:"dir/file",Size:new(int64(3)),Progress:1}},func(string)error{return errors.New("wrong filesystem")}); err==nil { t.Fatal("wrong filesystem accepted") }
}

func TestTranslate(t *testing.T) {
 t.Parallel()
 h:=handler{cfg:Config{VirtualPrefix:"/data",PhysicalData:"/mnt/disk4/data"}}
 for _,tt:=range []struct{in,want string; bad bool}{{"/data/tv/a","/mnt/disk4/data/tv/a",false},{"/data","/mnt/disk4/data",false},{"/data2/file","",true},{"/data/../secret","",true},{"relative","",true}} {
  t.Run(tt.in,func(t *testing.T){t.Parallel(); got,err:=h.translate(tt.in); if (err!=nil)!=tt.bad || got!=tt.want {t.Fatalf("translate = %q, %v",got,err)} })
 }
}

func TestAllowedEndpoints(t *testing.T) {
 t.Parallel()
 upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){w.WriteHeader(http.StatusNoContent)})); defer upstream.Close()
 hash:=strings.Repeat("a",40)
 h,err:=New(Config{Listen:":0",Upstream:upstream.URL,TorrentHash:hash,VirtualPrefix:"/data",PhysicalData:"/mnt/disk4/data"}); if err!=nil {t.Fatal(err)}
 for _,tt:=range []struct{method,path string; status int}{
  {"POST","/api/v2/auth/login",204}, {"GET","/api/v2/app/version",204},
  {"POST","/api/v2/torrents/delete",403}, {"GET","/api/v2/torrents/delete",403},
  {"POST","/api/v2/torrents/info",403}, {"GET","/api/v2/torrents/files?hash="+strings.Repeat("b",40),403},
  {"GET","/api/v2/torrents/files?hash="+hash,204},
 } { t.Run(tt.method+tt.path,func(t *testing.T){req:=httptest.NewRequest(tt.method,tt.path,nil); w:=httptest.NewRecorder(); h.ServeHTTP(w,req); if w.Code!=tt.status {t.Fatalf("got status %d, want %d",w.Code,tt.status)} }) }
}

func TestSyncIncrementalAndLogin(t *testing.T) {
 t.Parallel()
 hash:=strings.Repeat("a",40)
 upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  switch r.URL.Path {
  case "/api/v2/auth/login": http.SetCookie(w,&http.Cookie{Name:"SID",Value:"test",Path:"/"}); w.Write([]byte("Ok."))
  case "/api/v2/torrents/info":
   if _,err:=r.Cookie("SID"); err!=nil { http.Error(w,"forbidden",403); return }
   w.Header().Set("Content-Type","application/json"); w.Write([]byte(`[{"hash":"`+hash+`","save_path":"/data/tv","progress":1}]`))
  case "/api/v2/torrents/files": w.Write([]byte(`[{"name":"movie","size":3,"progress":1}]`))
  case "/api/v2/sync/maindata": w.Write([]byte(`{"rid":42,"full_update":false,"torrents":{"`+hash+`":{"save_path":"/data/tv","state":"uploading"}}}`))
  default: http.NotFound(w,r)
  }
 })); defer upstream.Close()
 raw,err:=New(Config{Listen:":0",Upstream:upstream.URL,TorrentHash:hash,VirtualPrefix:"/data",PhysicalData:"/mnt/disk4/data"}); if err!=nil {t.Fatal(err)}
 h:=raw.(*handler); h.verifyTorrent=func(string,[]file)error{return nil}
 login:=httptest.NewRecorder(); h.ServeHTTP(login,httptest.NewRequest("POST","/api/v2/auth/login",nil)); cookies:=login.Result().Cookies(); if len(cookies)!=1 {t.Fatal("login cookie not forwarded")}
 req:=httptest.NewRequest("GET","/api/v2/sync/maindata?rid=41",nil); req.AddCookie(cookies[0]); w:=httptest.NewRecorder(); h.ServeHTTP(w,req)
 if w.Code!=200 || !strings.Contains(w.Body.String(),`"rid":42`) || !strings.Contains(w.Body.String(),`"full_update":false`) || !strings.Contains(w.Body.String(),`"save_path":"/mnt/disk4/data/tv"`) { t.Fatalf("unexpected sync response: %d %s",w.Code,w.Body.String()) }
 h.verifyTorrent=func(string,[]file)error{return errors.New("disk missing")}; w=httptest.NewRecorder(); h.ServeHTTP(w,req); if w.Code!=502 {t.Fatalf("verification failure returned %d",w.Code)}
}
