package devtools

import (
	"log"
	"os"
	"testing"
	"time"

<<<<<<< HEAD
	"github.com/tidwall/gjson"
	"github.com/vinhjaxt/devtools"
=======
	"github.com/tidwall/sjson"
>>>>>>> 0fed03321c718e5d1d1acb4ac1e21c35846fe375
)

func TestDemo(t *testing.T) {
	dv, err := NewDevtools("http://localhost:9222")
	if err != nil {
		log.Panicln(err)
	}

	ss, err := dv.NewSession("about:blank")
	if err != nil {
		log.Panicln(err)
	}
	log.Println("TargetID", ss.TargetID)

	sjson.Set(`{"method":"Page.addScriptToEvaluateOnNewDocument","params":{"source":""}}`, "params.source", `
	window.alert = function alert(){
		// prevent
	}
	`)

	ss.WaitNavigate("http://google.com")
	// You can execJS or wait for some front-end here
	ss.WaitJSExecCTX(5 * time.Second)
	ss.ExecJs(`1`)
	log.Println("Done")
	os.Exit(0)
}

func autoCloseTab(s *devtools.Session, dv *devtools.DevtoolsConn) {
	eID := s.AddEvent(func(body *gjson.Result, err error) {
		if targetID := body.Get("params.targetInfo.targetId").String(); body.Get("method").String() == "Target.targetCreated" && body.Get("params.targetInfo.openerId").String() == s.TargetID && targetID != "" {
			// new tab created by this target
			dv.CloseTab(targetID)
		}
	})
	defer s.DelEvent(eID)
}
