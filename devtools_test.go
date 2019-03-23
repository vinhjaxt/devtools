package devtools

import (
	"log"
	"os"
	"testing"
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

	ss.WaitNavigate("http://google.com")
	// You can execJS or wait for some front-end here
	ss.ExecJs(`1`)
	log.Println("Done")
	os.Exit(0)
}
