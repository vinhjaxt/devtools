package devtools

import (
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/chromedp/kb"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/fasthttp/websocket"
)

// SessionMeta contains URL of tab
type SessionMeta struct {
	Url string
}

// Session hold tab connection
type Session struct {
	nextSendID   uint64
	Dv           *DevtoolsConn
	TargetID     string
	Conn         *websocket.Conn
	ConnMu       *sync.Mutex
	IsClosed     *atomic.Value
	fEvents      map[uint32]func(*gjson.Result, error)
	feMu         *sync.RWMutex
	feCount      uint32
	Meta         *SessionMeta
	OnNavigate   func(url string)
	Loading      *sync.WaitGroup
	Navigating   int32
	IsJSCxtReady int32
	ExecCxtID    int32
}

// OpenSession open session by targetId (tabId)
func (dv *DevtoolsConn) OpenSession(targetID string) (*Session, error) {
	wsURL, err := url.Parse(dv.Url)
	if err != nil {
		return nil, err
	}
	wsURL.Path = "/devtools/page/" + url.PathEscape(targetID)
	wsURL.Scheme = "ws"
	c, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		return nil, err
	}

	var isClosed atomic.Value
	isClosed.Store(false)
	s := &Session{
		nextSendID: 0,
		Dv:         dv,
		TargetID:   targetID,
		IsClosed:   &isClosed,
		Conn:       c,
		ConnMu:     &sync.Mutex{},
		fEvents:    map[uint32]func(*gjson.Result, error){},
		feMu:       &sync.RWMutex{},
		feCount:    0,
		Meta:       &SessionMeta{},
		Loading:    &sync.WaitGroup{},
		Navigating: 0,
	}

	for _, initCmd := range []string{`{"method":"Page.enable"}`, `{"method":"Network.enable"}`, `{"method":"Target.enable"}`, `{"method":"Runtime.enable"}`, `{method":"Target.setAutoAttach","params":{"autoAttach":true,"waitForDebuggerOnStart":true,"flatten":true}}`, `{"method":"Target.setDiscoverTargets","params":{"discover":true}}`} {
		err = s.WriteCommand(initCmd)
		if err != nil {
			return nil, err
		}
	}

	go func() {
		for {
			_, body, err := c.ReadMessage()
			if err != nil {
				isClosed.Store(true)
				c.Close()
				break
			}
			json := gjson.ParseBytes(body)
			go s.broadcastTarget(&json, err)
			go s.processEvent(&json)
		}
	}()

	return s, nil
}

func (s *Session) broadcastTarget(body *gjson.Result, err error) {
	s.feMu.RLock()
	for _, fn := range s.fEvents {
		go fn(body, err)
	}
	s.feMu.RUnlock()
}

// AddEvent listen for ws response
func (s *Session) AddEvent(fn func(body *gjson.Result, err error)) uint32 {
	feID := atomic.AddUint32(&s.feCount, 1)
	s.feMu.Lock()
	s.fEvents[feID] = fn
	s.feMu.Unlock()
	return feID
}

// DelEvent by AddEvent
func (s *Session) DelEvent(fID uint32) {
	s.feMu.Lock()
	for k := range s.fEvents {
		if k == fID {
			delete(s.fEvents, k)
			break
		}
	}
	s.feMu.Unlock()
}

// Target.targetInfoChanged: {"method":"Target.targetInfoChanged","params":{"targetInfo":{"targetId":"39047F6BC601065749331FCE34A1D2CE","type":"page","title":"v88.be","url":"http://v88.be/","attached":true,"browserContextId":"9FA87D43187C093D7516D211A52BE4A5"}}}
func (s *Session) processEvent(json *gjson.Result) {
	method := json.Get("method").String()
	// log.Println(json.Raw)
	if method != "" {
		switch method {
		case "Runtime.executionContextCreated":
			if json.Get("params.context.auxData.frameId").String() == s.TargetID && json.Get("params.context.auxData.isDefault").Bool() {
				// isDefault false => extension context
				// log.Println("JS Ctx created")
				atomic.StoreInt32(&s.IsJSCxtReady, 1)
				atomic.StoreInt32(&s.ExecCxtID, int32(json.Get("params.context.id").Int()))
			}
		case "Runtime.executionContextsCleared":
			// log.Println("JS Ctx Cleared")
			atomic.StoreInt32(&s.IsJSCxtReady, 0)
		case "Runtime.executionContextDestroyed":
			if int32(json.Get("params.executionContextId").Int()) == atomic.LoadInt32(&s.ExecCxtID) {
				// log.Println("JS Ctx Destroyed")
				atomic.StoreInt32(&s.IsJSCxtReady, 0)
			}
			// frameId
		case "Target.targetCreated":
			// new tab opened
			// body.Get("params.targetInfo.openerId").String() == s.TargetID => open by this tab
		case "Page.frameStartedLoading":
			if json.Get("params.frameId").String() == s.TargetID {
				s.Loading.Add(1)
				atomic.StoreInt32(&s.Navigating, 1)
			}
		case "Page.frameStoppedLoading":
			if json.Get("params.frameId").String() == s.TargetID {
				s.Loading.Add(-1)
				atomic.StoreInt32(&s.Navigating, 0)
			}
		case "Target.targetInfoChanged":
			if json.Get("params.targetInfo.targetId").String() == s.TargetID {
				url := json.Get("params.targetInfo.url").String()
				if url != s.Meta.Url {
					s.Meta.Url = url
					if s.OnNavigate != nil {
						s.OnNavigate(s.Meta.Url)
					}
				}
			}
		case "Page.loadEventFired", "Page.domContentEventFired":
		case "Page.navigatedWithinDocument":
			if json.Get("params.frameId").String() == s.TargetID {
				url := json.Get("params.url").String()
				if url != s.Meta.Url {
					s.Meta.Url = url
					if s.OnNavigate != nil {
						s.OnNavigate(s.Meta.Url)
					}
				}
			}
		case "Page.frameScheduledNavigation":
			if json.Get("params.frameId").String() == s.TargetID {
				atomic.StoreInt32(&s.Navigating, 1)
				url := json.Get("params.url").String()
				if url != s.Meta.Url {
					s.Meta.Url = url
					if s.OnNavigate != nil {
						s.OnNavigate(s.Meta.Url)
					}
				}
			}
		case "Page.frameNavigated":
			if json.Get("params.frame.id").String() == s.TargetID {
				atomic.StoreInt32(&s.Navigating, 0)
				url := json.Get("params.frame.url").String()
				if url != s.Meta.Url {
					s.Meta.Url = url
					if s.OnNavigate != nil {
						s.OnNavigate(s.Meta.Url)
					}
				}
			}
		}
	}
}

// WaitCommand by a function with timeout
func (s *Session) WaitCommand(cmd string, fn func(sentID uint64, body *gjson.Result, successSig chan struct{}), timeout time.Duration) error {
	var sentID uint64
	successSig := make(chan struct{}, 1)
	errSig := make(chan error, 1)
	defer s.DelEvent(s.AddEvent(func(body *gjson.Result, err error) {
		if err != nil {
			errSig <- err
			return
		}
		if sentID != 0 {
			fn(sentID, body, successSig)
		}
	}))
	go func() {
		var err error
		sentID, err = s.WriteCommandID(cmd)
		if err != nil {
			errSig <- err
		}
	}()
	select {
	case <-successSig:
		return nil
	case err := <-errSig:
		return err
	case <-time.After(timeout):
		return errors.New("Timeout WaitCommand: " + cmd)
	}
}

// Navigate to link dont wait complete
func (s *Session) Navigate(link string) error {
	success := make(chan struct{}, 1)
	errSig := make(chan error, 1)
	defer s.DelEvent(s.AddEvent(func(body *gjson.Result, err error) {
		if err != nil {
			errSig <- err
			return
		}
		if body.Get("method").String() == "Page.frameStartedLoading" && body.Get("params.frameId").String() == s.TargetID {
			success <- struct{}{}
		}
	}))
	go func() {
		err := s.WriteCommand(`{"method":"Page.navigate","params":{"url":` + jsonEncode(link) + `}}`)
		if err != nil {
			errSig <- err
		}
	}()
	select {
	case err := <-errSig:
		return err
	case <-success:
		return nil
	case <-time.After(15 * time.Second):
		return errors.New("Timeout navigate to: " + link)
	}
}

// WaitNavigate to link dont wait complete
func (s *Session) WaitNavigate(link string) error {
	err := s.Navigate(link)
	if err != nil {
		return err
	}
	return s.WaitNavigating(15 * time.Second)
}

// WaitNavigateComplete to a url
func (s *Session) WaitNavigateComplete(link string) error {
	err := s.Navigate(link)
	if err != nil {
		return err
	}
	return s.WaitLoading(15 * time.Second)
}

// Close session
func (s *Session) Close(closeTab bool) error {
	s.IsClosed.Store(true)
	err := s.Conn.Close()
	if closeTab {
		err2 := s.Dv.CloseTab(s.TargetID)
		if err == nil {
			err = err2
		}
	}
	s.fEvents = nil
	s.Meta = nil
	s.Dv = nil
	s.Conn = nil
	s.feMu = nil
	return err
}

// SendCommand and wait for response
func (s *Session) SendCommand(json string) (*gjson.Result, error) {
	if s.IsClosed.Load().(bool) {
		return nil, errors.New("Tab closed")
	}

	sentID := atomic.AddUint64(&s.nextSendID, 1)
	json, err := sjson.Set(json, "id", sentID)
	if err != nil {
		return nil, err
	}

	success := make(chan *gjson.Result, 1)
	errSig := make(chan error, 1)
	defer s.DelEvent(s.AddEvent(func(body *gjson.Result, err error) {
		if err != nil {
			errSig <- err
		}
		if body.Get("id").Uint() == sentID {
			success <- body
		}
	}))

	go func() {
		s.ConnMu.Lock()
		err = s.Conn.WriteMessage(websocket.TextMessage, []byte(json))
		s.ConnMu.Unlock()
		if err != nil {
			s.IsClosed.Store(true)
			s.Conn.Close()
			errSig <- err
		}
	}()

	select {
	case err := <-errSig:
		return nil, err
	case result := <-success:
		return result, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("Timeout response")
	}
}

// WaitLoading for complete
func (s *Session) WaitLoading(timeout time.Duration) error {
	success := make(chan struct{}, 1)
	go func() {
		s.Loading.Wait()
		success <- struct{}{}
	}()
	select {
	case <-success:
		return nil
	case <-time.After(timeout):
		return errors.New("WaitLoading Timeout")
	}
}

// WaitNavigating for html ready (stopped loading)
func (s *Session) WaitNavigating(timeout time.Duration) error {
	if atomic.LoadInt32(&s.Navigating) == 0 {
		return nil
	}
	success := make(chan struct{}, 1)
	go func() {
		for atomic.LoadInt32(&s.Navigating) != 0 {
			time.Sleep(100 * time.Millisecond)
		}
		success <- struct{}{}
	}()
	select {
	case <-success:
		return nil
	case <-time.After(timeout):
		return errors.New("WaitNavigating Timeout")
	}
}

// WaitJSExecCTX for javascript execution context ready
func (s *Session) WaitJSExecCTX(timeout time.Duration) error {
	if atomic.LoadInt32(&s.IsJSCxtReady) != 0 {
		return nil
	}
	success := make(chan struct{}, 1)
	go func() {
		for atomic.LoadInt32(&s.IsJSCxtReady) == 0 {
			time.Sleep(100 * time.Millisecond)
		}
		success <- struct{}{}
	}()
	select {
	case <-success:
		return nil
	case <-time.After(timeout):
		return errors.New("WaitJSExecCTX Timeout")
	}
}

// WriteCommand and not wait for response
func (s *Session) WriteCommand(json string) error {
	if s.IsClosed.Load().(bool) {
		return errors.New("Tab closed")
	}
	sentID := atomic.AddUint64(&s.nextSendID, 1)
	json, err := sjson.Set(json, "id", sentID)
	if err != nil {
		return err
	}
	s.ConnMu.Lock()
	err = s.Conn.WriteMessage(websocket.TextMessage, []byte(json))
	s.ConnMu.Unlock()
	if err != nil {
		s.IsClosed.Store(true)
		s.Conn.Close()
		return err
	}
	return nil
}

// WriteCommandID return commandID and not wait for response
func (s *Session) WriteCommandID(json string) (uint64, error) {
	if s.IsClosed.Load().(bool) {
		return 0, errors.New("Tab closed")
	}
	sentID := atomic.AddUint64(&s.nextSendID, 1)
	json, err := sjson.Set(json, "id", sentID)
	if err != nil {
		return 0, err
	}
	s.ConnMu.Lock()
	err = s.Conn.WriteMessage(websocket.TextMessage, []byte(json))
	s.ConnMu.Unlock()
	if err != nil {
		s.IsClosed.Store(true)
		s.Conn.Close()
		return 0, err
	}
	return sentID, nil
}

// ExecJsPromise execute js promise response eg: (async () => await new Promise((resolve, reject) => { ... }))()
func (s *Session) ExecJsPromise(js string) (*gjson.Result, error) {
	json, err := sjson.Set(`{"method":"Runtime.evaluate","params":{"expression":"","objectGroup":"console","includeCommandLineAPI":true,"returnByValue":false,"generatePreview":false,"awaitPromise":true}}`, "params.expression", js)
	if err != nil {
		return nil, err
	}
	return s.SendCommand(json)
}

// ExecJs normal js code and wait for return
func (s *Session) ExecJs(js string) (*gjson.Result, error) {
	json, err := sjson.Set(`{"method":"Runtime.evaluate","params":{"expression":"","objectGroup":"console","includeCommandLineAPI":true,"returnByValue":false,"generatePreview":false,"awaitPromise":false}}`, "params.expression", js)
	if err != nil {
		return nil, err
	}
	return s.SendCommand(json)
}

// ClearBrowserCache of this context
func (s *Session) ClearBrowserCache() error {
	return s.WriteCommand(`{"method":"Network.clearBrowserCache"}`)
}

// ClearBrowserCookies of this context
func (s *Session) ClearBrowserCookies() error {
	return s.WriteCommand(`{"method":"Network.clearBrowserCookies"}`)
}

// WaitReady wait for selector exist
func (s *Session) WaitReady(sel string) error {
	json, err := s.ExecJsPromise(`
	(async () => {
		return (await new Promise((resolve, reject) => {
			const v = document.querySelector(` + jsonEncode(sel) + `)
			if (v) return resolve(v)
			const timeout = setTimeout(() => {
				clearInterval(ticker)
				reject(new Error("No element found"))
			}, 8000)
			const ticker = setInterval(() => {
				const v = document.querySelector(` + jsonEncode(sel) + `)
				if (v) {
					clearTimeout(timeout)
					clearInterval(ticker)
					resolve(v)
				}
			}, 200)
		}))
	})()
	`)
	if err != nil {
		return err
	}
	if json.Get("result.result.subtype").String() == "node" {
		return nil
	}
	return errors.New("Can not find " + sel + ": " + json.Raw)
}

// WaitHidden wait for selector hidden or not exist
func (s *Session) WaitHidden(sel string) error {
	json, err := s.ExecJsPromise(`
	(async () => {
		return (await new Promise((resolve, reject) => {
			const v = document.querySelector(` + jsonEncode(sel) + `)
			if (!v || window.getComputedStyle(v).display === "none") return resolve(true)
			const timeout = setTimeout(() => {
				clearInterval(ticker)
				reject(new Error("No element found"))
			}, 8000)
			const ticker = setInterval(() => {
				const v = document.querySelector(` + jsonEncode(sel) + `)
				if (!v || window.getComputedStyle(v).display === "none") {
					clearTimeout(timeout)
					clearInterval(ticker)
					resolve(true)
				}
			}, 200)
		}))
	})()
	`)
	if err != nil {
		return err
	}
	if json.Get("result.result.type").String() == "boolean" {
		return nil
	}
	return errors.New("Can not find " + sel)
}

// WaitShow wait for selector exist and not hidden
func (s *Session) WaitShow(sel string) error {
	json, err := s.ExecJsPromise(`
	(async () => {
		return (await new Promise((resolve, reject) => {
			const v = document.querySelector(` + jsonEncode(sel) + `)
			if (v && window.getComputedStyle(v).display !== "none") return resolve(true)
			const timeout = setTimeout(() => {
				clearInterval(ticker)
				reject(new Error("No element found"))
			}, 8000)
			const ticker = setInterval(() => {
				const v = document.querySelector(` + jsonEncode(sel) + `)
				if (v && window.getComputedStyle(v).display !== "none") {
					clearTimeout(timeout)
					clearInterval(ticker)
					resolve(true)
				}
			}, 200)
		}))
	})()
	`)
	if err != nil {
		return err
	}
	if json.Get("result.result.type").String() == "boolean" {
		return nil
	}
	return errors.New("Can not find " + sel)
}

// SetAttribute set attribute of selector
func (s *Session) SetAttribute(sel, key string, val interface{}) error {
	json, err := s.ExecJs(`document.querySelector(` + jsonEncode(sel) + `).setAttribute(` + jsonEncode(key) + `, ` + jsonEncode(val) + `)`)
	if err != nil {
		return err
	}
	if json.Get("result.result.subtype").String() == "error" {
		return errors.New("Can not set attribute: " + sel + " <- " + key)
	}
	return nil
}

// SetAttributeDirect set attribute directly of selector
func (s *Session) SetAttributeDirect(sel, key string, val interface{}) error {
	json, err := s.ExecJs(`document.querySelector(` + jsonEncode(sel) + `)[` + jsonEncode(key) + `]=` + jsonEncode(val))
	if err != nil {
		return err
	}
	if json.Get("result.result.subtype").String() == "error" {
		return errors.New("Can not set attribute: " + sel + " <- " + key)
	}
	return nil
}

// Exist check if selector exist
func (s *Session) Exist(sel string) (bool, error) {
	json, err := s.ExecJs(`document.querySelector(` + jsonEncode(sel) + `)`)
	if err != nil {
		return false, err
	}
	return json.Get("result.result.subtype").String() == "node", nil
}

// WaitRemove wait for selector not exist any more
func (s *Session) WaitRemove(sel string) error {
	json, err := s.ExecJsPromise(`
	(async () => {
		return (await new Promise((resolve, reject) => {
			const v = document.querySelector(` + jsonEncode(sel) + `)
			if (!v) return resolve()
			const timeout = setTimeout(() => {
				clearInterval(ticker)
				reject(new Error("No element found"))
			}, 8000)
			const ticker = setInterval(() => {
				const v = document.querySelector(` + jsonEncode(sel) + `)
				if (!v) {
					clearTimeout(timeout)
					clearInterval(ticker)
					resolve()
				}
			}, 200)
		}))
	})()
	`)
	if err != nil {
		return err
	}
	if json.Get("result.result.type").String() == "undefined" {
		return nil
	}
	return errors.New("Can not find " + sel)
}

// WaitSendKeys wait for send keys
func (s *Session) WaitSendKeys(sel, chars string) error {
	json, err := s.ExecJs(`document.querySelector(` + jsonEncode(sel) + `).focus()`)
	if err != nil {
		return err
	}
	if json.Get("result.result.type").String() != "undefined" {
		return errors.New("Can not focus element to SendKeys: " + sel)
	}
	return s.JustSendKeys(chars)
}

// GetResponseBody get response body of request id
func (s *Session) GetResponseBody(requestID string) (body string, isBase64 bool, err error) {
	json, err := s.SendCommand(`{"method":"Network.getResponseBody","params":{"requestId":` + jsonEncode(requestID) + `}}`)
	if err != nil {
		return
	}
	body = json.Get("result.body").String()
	isBase64 = json.Get("result.base64Encoded").Bool()
	return
}

// WaitRequest wait for a request by match function and init by init function, return request success or failed, request id or error, url: params.request.url
func (s *Session) WaitRequest(match func(req *gjson.Result) bool, init func() error) (bool, string, error) {
	success := make(chan bool, 1)
	errChan := make(chan error, 1)

	requestID := ""

	defer s.DelEvent(s.AddEvent(func(body *gjson.Result, err error) {
		if err != nil {
			errChan <- err
			return
		}
		switch body.Get("method").String() {
		case "Network.requestWillBeSent":
			if requestID == "" && match(body) {
				requestID = body.Get("params.requestId").String()
			}
		case "Network.loadingFinished":
			if requestID == body.Get("params.requestId").String() {
				success <- true
			}
		case "Network.loadingFailed":
			if requestID == body.Get("params.requestId").String() {
				success <- false
			}
		}
	}))

	go func() {
		if err := init(); err != nil {
			errChan <- err
		}
	}()

	select {
	case result := <-success:
		return result, requestID, nil
	case err := <-errChan:
		return false, requestID, err
	case <-time.After(15 * time.Second):
		return false, requestID, errors.New("Timeout request")
	}
}

// JustSendKeys send keys
func (s *Session) JustSendKeys(chars string) error {
	for _, k := range chars {
		for _, key := range kb.Encode(k) {
			b, err := key.MarshalJSON()
			if err != nil {
				return err
			}
			val, err := sjson.SetRaw(`{"method":"Input.dispatchKeyEvent","params":null}`, "params", string(b))
			if err != nil {
				return err
			}
			err = s.WriteCommand(val)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Click to selector
func (s *Session) Click(sel string) error {
	json, err := s.ExecJs(`document.querySelector(` + jsonEncode(sel) + `).click()`)
	if err != nil {
		return err
	}
	if json.Get("result.result.type").String() != "undefined" {
		return errors.New("Can not Click: " + sel)
	}
	return nil
}

// Active the target
func (s *Session) Active() error {
	return s.WriteCommand(`{"method":"Page.bringToFront"}`)
}
