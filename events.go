package devtools

import (
	"log"
	"sync/atomic"

	"github.com/tidwall/gjson"
)

// Target.targetInfoChanged: {"method":"Target.targetInfoChanged","params":{"targetInfo":{"targetId":"39047F6BC601065749331FCE34A1D2CE","type":"page","title":"v88.be","url":"http://v88.be/","attached":true,"browserContextId":"9FA87D43187C093D7516D211A52BE4A5"}}}
func (s *Session) processEvent(json *gjson.Result) {
	method := json.Get("method").String()
	// log.Println(json.Raw)
	// log.Println(method)
	if method != "" {
		switch method {
		case "Page.lifecycleEvent":
			{
				if json.Get("params.frameId").String() == s.TargetID {
					name := json.Get("params.name").String()
					if name == "init" {
						s.lifecycleEvents.Clear()
					}
					s.lifecycleEvents.Add(name)
				}
				log.Println(s.lifecycleEvents.String())
				break
			}
		case "Runtime.executionContextCreated":
			{
				if json.Get("params.context.auxData.frameId").String() == s.TargetID && json.Get("params.context.auxData.isDefault").Bool() {
					// isDefault false => extension context
					// log.Println("JS Ctx created")
					atomic.StoreInt32(&s.IsJSCxtReady, 1)
					atomic.StoreInt32(&s.ExecCxtID, int32(json.Get("params.context.id").Int()))
				}
				break
			}
		case "Runtime.executionContextsCleared":
			{
				// log.Println("JS Ctx Cleared")
				atomic.StoreInt32(&s.IsJSCxtReady, 0)
				break
			}
		case "Runtime.executionContextDestroyed":
			{
				if int32(json.Get("params.executionContextId").Int()) == atomic.LoadInt32(&s.ExecCxtID) {
					// log.Println("JS Ctx Destroyed")
					atomic.StoreInt32(&s.IsJSCxtReady, 0)
				}
				// frameId
				break
			}
		case "Target.targetCreated":
			{
				// new tab opened
				// body.Get("params.targetInfo.openerId").String() == s.TargetID => open by this tab
				break
			}
		case "Page.frameStoppedLoading":
			{
				if json.Get("params.frameId").String() == s.TargetID {
					log.Println(s.lifecycleEvents.String())
					s.lifecycleEvents.Add("DOMContentLoaded")
					s.lifecycleEvents.Add("load")
				}
				break
			}
		case "Target.targetInfoChanged":
			{
				if json.Get("params.targetInfo.targetId").String() == s.TargetID {
					url := json.Get("params.targetInfo.url").String()
					if url != s.Meta.URL {
						s.Meta.URL = url
						if s.OnNavigate != nil {
							s.OnNavigate(s.Meta.URL)
						}
					}
				}
				break
			}
		case "Page.navigatedWithinDocument":
			{
				if json.Get("params.frameId").String() == s.TargetID {
					url := json.Get("params.url").String()
					if url != s.Meta.URL {
						s.Meta.URL = url
						if s.OnNavigate != nil {
							s.OnNavigate(s.Meta.URL)
						}
					}
				}
				break
			}
		case "Page.frameScheduledNavigation":
			{
				if json.Get("params.frameId").String() == s.TargetID {
					url := json.Get("params.url").String()
					if url != s.Meta.URL {
						s.Meta.URL = url
						if s.OnNavigate != nil {
							s.OnNavigate(s.Meta.URL)
						}
					}
				}
				break
			}
		case "Page.frameNavigated":
			{
				if json.Get("params.frame.id").String() == s.TargetID {
					url := json.Get("params.frame.url").String()
					if url != s.Meta.URL {
						s.Meta.URL = url
						if s.OnNavigate != nil {
							s.OnNavigate(s.Meta.URL)
						}
					}
				}
				break
			}
		}
	}
}
