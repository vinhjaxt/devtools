package devtools

import (
	"log"

	"github.com/json-iterator/go"
)

var json = jsoniter.ConfigCompatibleWithStandardLibrary

func jsonEncode(v interface{}) string {
	d, err := json.MarshalToString(v)
	if err != nil {
		log.Println(err)
	}
	return d
}
