package devtools_test

import (
	"log"
	"os"
	"testing"

	"./"
)

func TestDemo(t *testing.T) {
	dv, err := devtools.NewDevtools("http://localhost:9222")
	if err != nil {
		log.Panicln(err)
	}

	ss, err := dv.NewSession("about:blank")
	if err != nil {
		log.Panicln(err)
	}
	log.Println("TargetID", ss.TargetID)

	ss.WaitNavigate("http://google.com")
	ss.Loading.Wait()
	ss.Loading.Wait()
	ss.Loading.Wait()
	log.Println("Done")
	os.Exit(0)
}
