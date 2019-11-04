package httpopera

import (
	"encoding/json"
	"fmt"
	"github.com/gwuhaolin/livego/protocol/rtmp/rtmprelay"
	"io"
	"net"
	"net/http"
	"log"
	"github.com/gwuhaolin/livego/av"
	"github.com/gwuhaolin/livego/protocol/rtmp"
	"os/exec"
	"os"
	"path/filepath"
	"strings"
	"errors"
	"github.com/gwuhaolin/livego/protocol/httpflv"
	"github.com/gwuhaolin/livego/protocol/hls"
)
func getCurrentPath() (string, error) {
	file, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	i := strings.LastIndex(path, "/")
	if i < 0 {
		i = strings.LastIndex(path, "\\")
	}
	if i < 0 {
		return "", errors.New(`error: Can't find "/" or "\".`)
	}
	return string(path[0 : i+1]), nil
}

type Response struct {
	w       http.ResponseWriter
	Status  int    `json:"status"`
	Message string `json:"message"`
}

func (r *Response) SendJson() (int, error) {
	resp, _ := json.Marshal(r)
	r.w.Header().Set("Content-Type", "application/json")
	return r.w.Write(resp)
}

type Operation struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Stop   bool   `json:"stop"`
}

type OperationChange struct {
	Method    string `json:"method"`
	SourceURL string `json:"source_url"`
	TargetURL string `json:"target_url"`
	Stop      bool   `json:"stop"`
}

type ClientInfo struct {
	url              string
	rtmpRemoteClient *rtmp.Client
	rtmpLocalClient  *rtmp.Client
}

type Server struct {
	handler  av.Handler
	session  map[string]*rtmprelay.RtmpRelay
	rtmpAddr string

	flvHandler interface{}
	hlsHandler interface{}
}

func NewServer(h av.Handler, rtmpAddr string,flv interface{},hls interface{}) *Server {
	return &Server{
		handler:  h,
		session:  make(map[string]*rtmprelay.RtmpRelay),
		rtmpAddr: rtmpAddr,
		flvHandler:flv,
		hlsHandler:hls,
	}
}
var crossdomainxml = []byte(`<?xml version="1.0" ?>
<cross-domain-policy>
	<allow-access-from domain="*" />
	<allow-http-request-headers-from domain="*" headers="*"/>
</cross-domain-policy>`)
func (s *Server) Serve(l net.Listener) error {

	mux := http.NewServeMux()

	mux.HandleFunc("/crossdomain.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Write(crossdomainxml)
		return
	})

	mux.Handle("/web/",http.StripPrefix("/web",http.FileServer(http.Dir("./web/"))))
	mux.HandleFunc("/control/push", func(w http.ResponseWriter, r *http.Request) {
		s.handlePush(w, r)
	})
	mux.HandleFunc("/control/pull", func(w http.ResponseWriter, r *http.Request) {
		s.handlePull(w, r)
	})
	mux.HandleFunc("/stat/livestat", func(w http.ResponseWriter, r *http.Request) {
		s.GetLiveStatics(w, r)
	})
	mux.HandleFunc("/play/", func(w http.ResponseWriter, r *http.Request) {
		s.StartPlay(w, r)
	})

	http.Serve(l, mux)
	return nil
}

type stream struct {
	Key             string `json:"key"`
	Url             string `json:"Url"`
	StreamId        uint32 `json:"StreamId"`
	VideoTotalBytes uint64 `json:123456`
	VideoSpeed      uint64 `json:123456`
	AudioTotalBytes uint64 `json:123456`
	AudioSpeed      uint64 `json:123456`
}

type streams struct {
	Publishers []stream `json:"publishers"`
	Players    []stream `json:"players"`
}

//http://127.0.0.1:8090/stat/livestat
func (server *Server) GetLiveStatics(w http.ResponseWriter, req *http.Request) {
	rtmpStream := server.handler.(*rtmp.RtmpStream)
	if rtmpStream == nil {
		io.WriteString(w, "<h1>Get rtmp stream information error</h1>")
		return
	}

	msgs := new(streams)
	for item := range rtmpStream.GetStreams().IterBuffered() {
		if s, ok := item.Val.(*rtmp.Stream); ok {
			if s.GetReader() != nil {
				switch s.GetReader().(type) {
				case *rtmp.VirReader:
					v := s.GetReader().(*rtmp.VirReader)
					msg := stream{item.Key, v.Info().URL, v.ReadBWInfo.StreamId, v.ReadBWInfo.VideoDatainBytes, v.ReadBWInfo.VideoSpeedInBytesperMS,
						v.ReadBWInfo.AudioDatainBytes, v.ReadBWInfo.AudioSpeedInBytesperMS}
					msgs.Publishers = append(msgs.Publishers, msg)
				}
			}
		}
	}

	for item := range rtmpStream.GetStreams().IterBuffered() {
		ws := item.Val.(*rtmp.Stream).GetWs()
		for s := range ws.IterBuffered() {
			if pw, ok := s.Val.(*rtmp.PackWriterCloser); ok {
				if pw.GetWriter() != nil {
					switch pw.GetWriter().(type) {
					case *rtmp.VirWriter:
						v := pw.GetWriter().(*rtmp.VirWriter)
						msg := stream{item.Key, v.Info().URL, v.WriteBWInfo.StreamId, v.WriteBWInfo.VideoDatainBytes, v.WriteBWInfo.VideoSpeedInBytesperMS,
							v.WriteBWInfo.AudioDatainBytes, v.WriteBWInfo.AudioSpeedInBytesperMS}
						msgs.Players = append(msgs.Players, msg)
					}
				}
			}
		}
	}
	resp, _ := json.Marshal(msgs)
	w.Header().Set("Content-Type", "application/json")
	w.Write(resp)
}

//http://127.0.0.1:8090/play/live/movie(.flv/.m3u8)
func (server*Server)  StartPlay(w http.ResponseWriter, req *http.Request) {
	defer func() {
		if r := recover(); r != nil {
			log.Println("http-opr flv handleConn panic: ", r)
		}
	}()
	url := req.URL.String()
	u := strings.TrimPrefix(req.URL.Path, "/play/")
	an := strings.Split(u, "/")
	log.Println("url:", url, "paths:", an)
	if len(an) < 2 || len(an[0]) == 0 || len(an[1]) == 0 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	app, name := an[0], an[1]
	tsname := ""
	ts := false
	if len(an) == 3 && strings.HasSuffix(an[2], ".ts") {
		ts = true
	}
	var suffix = ""
	if !ts {
		pos := strings.LastIndex(name, ".")
		if pos < 0 {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		suffix = name[pos:]
		name = name[0:pos]
		if len(name) == 0 {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
	} else {
		suffix = ".ts"
		tsname = strings.TrimSuffix(an[2], ".ts")
	}

	rtmpStream := server.handler.(*rtmp.RtmpStream)
	if rtmpStream == nil {
		http.Error(w, "null rtmp stream", http.StatusExpectationFailed)
		return
	}

	switch suffix {
	case ".flv":
		flvs := server.flvHandler.(*httpflv.Server)
		if (flvs == nil) {
			http.Error(w, "null flv server", http.StatusExpectationFailed)
			return
		}
		flvs.StartPlay(app, name, req.URL.String(), w)
		break
	case ".m3u8", ".ts":
		hlss := server.hlsHandler.(*hls.Server)
		if (hlss == nil) {
			http.Error(w, "null hls server", http.StatusExpectationFailed)
			return
		}
		hlss.StartPlay(app, name, tsname, suffix, req.URL.String(), w)
		break
	default:
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
}

//http://127.0.0.1:8090/control/push?&oper=start&app=live&name=123456&url=rtmp://192.168.16.136/live/123456
func (s *Server) handlePull(w http.ResponseWriter, req *http.Request) {
	var retString string
	var err error

	req.ParseForm()

	oper := req.Form["oper"]
	app := req.Form["app"]
	name := req.Form["name"]
	url := req.Form["url"]

	log.Printf("control pull: oper=%v, app=%v, name=%v, url=%v", oper, app, name, url)
	if (len(app) <= 0) || (len(name) <= 0) || (len(url) <= 0) {
		io.WriteString(w, "control push parameter error, please check them.</br>")
		return
	}

	remoteurl := "rtmp://127.0.0.1" + s.rtmpAddr + "/" + app[0] + "/" + name[0]
	localurl := url[0]

	keyString := "pull:" + app[0] + "/" + name[0]
	if oper[0] == "stop" {
		pullRtmprelay, found := s.session[keyString]

		if !found {
			retString = fmt.Sprintf("session key[%s] not exist, please check it again.", keyString)
			io.WriteString(w, retString)
			return
		}
		log.Printf("rtmprelay stop push %s from %s", remoteurl, localurl)
		pullRtmprelay.Stop()

		delete(s.session, keyString)
		retString = fmt.Sprintf("<h1>push url stop %s ok</h1></br>", url[0])
		io.WriteString(w, retString)
		log.Printf("pull stop return %s", retString)
	} else {
		pullRtmprelay := rtmprelay.NewRtmpRelay(&localurl, &remoteurl)
		log.Printf("rtmprelay start push %s from %s", remoteurl, localurl)
		err = pullRtmprelay.Start()
		if err != nil {
			retString = fmt.Sprintf("push error=%v", err)
		} else {
			s.session[keyString] = pullRtmprelay
			retString = fmt.Sprintf("<h1>push url start %s ok</h1></br>", url[0])
		}
		io.WriteString(w, retString)
		log.Printf("pull start return %s", retString)
	}
}

//http://127.0.0.1:8090/control/push?&oper=start&app=live&name=123456&url=rtmp://192.168.16.136/live/123456
func (s *Server) handlePush(w http.ResponseWriter, req *http.Request) {
	var retString string
	var err error

	req.ParseForm()

	oper := req.Form["oper"]
	app := req.Form["app"]
	name := req.Form["name"]
	url := req.Form["url"]

	log.Printf("control push: oper=%v, app=%v, name=%v, url=%v", oper, app, name, url)
	if (len(app) <= 0) || (len(name) <= 0) || (len(url) <= 0) {
		io.WriteString(w, "control push parameter error, please check them.</br>")
		return
	}

	localurl := "rtmp://127.0.0.1" + s.rtmpAddr + "/" + app[0] + "/" + name[0]
	remoteurl := url[0]

	keyString := "push:" + app[0] + "/" + name[0]
	if oper[0] == "stop" {
		pushRtmprelay, found := s.session[keyString]
		if !found {
			retString = fmt.Sprintf("<h1>session key[%s] not exist, please check it again.</h1>", keyString)
			io.WriteString(w, retString)
			return
		}
		log.Printf("rtmprelay stop push %s from %s", remoteurl, localurl)
		pushRtmprelay.Stop()

		delete(s.session, keyString)
		retString = fmt.Sprintf("<h1>push url stop %s ok</h1></br>", url[0])
		io.WriteString(w, retString)
		log.Printf("push stop return %s", retString)
	} else {
		pushRtmprelay := rtmprelay.NewRtmpRelay(&localurl, &remoteurl)
		log.Printf("rtmprelay start push %s from %s", remoteurl, localurl)
		err = pushRtmprelay.Start()
		if err != nil {
			retString = fmt.Sprintf("push error=%v", err)
		} else {
			retString = fmt.Sprintf("<h1>push url start %s ok</h1></br>", url[0])
			s.session[keyString] = pushRtmprelay
		}

		io.WriteString(w, retString)
		log.Printf("push start return %s", retString)
	}
}
