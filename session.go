package devtools

import (
	"errors"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	mapset "github.com/deckarep/golang-set"

	"github.com/chromedp/chromedp/kb"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/fasthttp/websocket"
)

// SessionMeta contains URL of tab
type SessionMeta struct {
	URL string
}

// Session hold tab connection
type Session struct {
	nextSendID      uint64
	Dv              *DevTools
	TargetID        string
	Conn            *websocket.Conn
	ConnMu          *sync.Mutex
	IsClosed        int32
	fEvents         map[uint32]func(*gjson.Result, error)
	feMu            *sync.RWMutex
	feCount         uint32
	Meta            *SessionMeta
	OnNavigate      func(url string)
	IsJSCxtReady    int32
	ExecCxtID       int32
	lifecycleEvents mapset.Set
}

// OpenSession open session by targetId (tabId)
func (dv *DevTools) OpenSession(targetID string) (*Session, error) {
	wsURL, err := url.Parse(dv.URL)
	if err != nil {
		return nil, err
	}
	wsURL.Path = "/devtools/page/" + url.PathEscape(targetID)
	wsURL.Scheme = "ws"
	c, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		return nil, err
	}

	s := &Session{
		nextSendID:      0,
		Dv:              dv,
		TargetID:        targetID,
		IsClosed:        0,
		Conn:            c,
		ConnMu:          &sync.Mutex{},
		fEvents:         map[uint32]func(*gjson.Result, error){},
		feMu:            &sync.RWMutex{},
		feCount:         0,
		Meta:            &SessionMeta{},
		lifecycleEvents: mapset.NewSet(),
	}

	time.Sleep(100 * time.Millisecond)
	for _, initCmd := range []string{`{"method":"Page.enable"}`, `{"method":"Network.enable"}`, `{"method":"Target.enable"}`, `{"method":"Runtime.enable"}`, `{method":"Target.setAutoAttach","params":{"autoAttach":true,"waitForDebuggerOnStart":false,"flatten":true}}`, `{"method":"Target.setDiscoverTargets","params":{"discover":true}}`, `{"method":"Page.setLifecycleEventsEnabled","params":{"enabled":true}}`} {
		time.Sleep(10 * time.Millisecond)
		err = s.WriteCommand(initCmd)
		if err != nil {
			return nil, err
		}
	}

	go func() {
		for {
			_, body, err := c.ReadMessage()
			if err != nil {
				atomic.StoreInt32(&s.IsClosed, 1)
				c.Close()
				break
			}
			json := gjson.ParseBytes(body)
			go s.processEvent(&json)
			go s.broadcastTarget(&json, err)
		}
	}()

	return s, nil
}

func (s *Session) broadcastTarget(body *gjson.Result, err error) {
	s.feMu.RLock()
	defer s.feMu.RUnlock()
	for _, fn := range s.fEvents {
		fn(body, err)
	}
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
	atomic.StoreInt32(&s.IsClosed, 1)
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
	if atomic.LoadInt32(&s.IsClosed) == 1 {
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
			atomic.StoreInt32(&s.IsClosed, 1)
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

// WaitLoading for complete (complete load html, but not sure for ready)
func (s *Session) WaitLoading(timeout time.Duration) error {
	if s.lifecycleEvents.Contains("DOMContentLoaded") {
		return nil
	}
	success := make(chan struct{}, 1)
	var isTimeout int32
	go func() {
		for s.lifecycleEvents.Contains("DOMContentLoaded") == false {
			time.Sleep(100 * time.Millisecond)
			if atomic.LoadInt32(&isTimeout) != 0 {
				return
			}
		}
		success <- struct{}{}
	}()
	select {
	case <-success:
		return nil
	case <-time.After(timeout):
		atomic.StoreInt32(&isTimeout, 1)
		return errors.New("WaitLoading Timeout")
	}
}

// WaitReady for html ready (stopped loading)
func (s *Session) WaitReady(timeout time.Duration) error {
	return s.WaitNavigating(timeout)
}

// WaitNavigating for html ready (stopped loading)
/*
lifecycleEvents mean
{
  'load': 'load',
  'domcontentloaded': 'DOMContentLoaded',
  'networkidle0': 'networkIdle',
  'networkidle2': 'networkAlmostIdle',
}
*/
func (s *Session) WaitNavigating(timeout time.Duration) error {
	if s.lifecycleEvents.Contains("networkIdle") {
		return nil
	}
	success := make(chan struct{}, 1)
	var isTimeout int32
	go func() {
		for s.lifecycleEvents.Contains("networkIdle") == false {
			time.Sleep(100 * time.Millisecond)
			if atomic.LoadInt32(&isTimeout) != 0 {
				return
			}
		}
		success <- struct{}{}
	}()
	select {
	case <-success:
		return nil
	case <-time.After(timeout):
		atomic.StoreInt32(&isTimeout, 1)
		// log.Println(atomic.LoadInt32(&s.Navigating))
		return errors.New("WaitNavigating Timeout")
	}
}

func (s *Session) WaitPageAvailable(timeout time.Duration) error {
	err := s.WaitLoading(timeout)
	if err != nil {
		return err
	}
	return s.WaitJSExecCTX(timeout)
}

// WaitJSExecCTX for javascript execution context ready
func (s *Session) WaitJSExecCTX(timeout time.Duration) error {
	if atomic.LoadInt32(&s.IsJSCxtReady) != 0 {
		return nil
	}
	success := make(chan struct{}, 1)
	var isTimeout int32
	go func() {
		for atomic.LoadInt32(&s.IsJSCxtReady) == 0 {
			time.Sleep(100 * time.Millisecond)
			if atomic.LoadInt32(&isTimeout) != 0 {
				return
			}
		}
		success <- struct{}{}
	}()
	select {
	case <-success:
		return nil
	case <-time.After(timeout):
		atomic.StoreInt32(&isTimeout, 1)
		return errors.New("WaitJSExecCTX Timeout")
	}
}

// WriteCommand and not wait for response
func (s *Session) WriteCommand(json string) error {
	if atomic.LoadInt32(&s.IsClosed) == 1 {
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
		atomic.StoreInt32(&s.IsClosed, 1)
		s.Conn.Close()
		return err
	}
	return nil
}

// WriteCommandID return commandID and not wait for response
func (s *Session) WriteCommandID(json string) (uint64, error) {
	if atomic.LoadInt32(&s.IsClosed) == 1 {
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
		atomic.StoreInt32(&s.IsClosed, 1)
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

// WaitSelReady wait for selector exist
func (s *Session) WaitSelReady(sel string) error {
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
	json, err := s.ExecJs(`if(true){const el = document.querySelector(` + jsonEncode(sel) + `);el[` + jsonEncode(key) + `]=` + jsonEncode(val) + `;el.setAttribute(` + jsonEncode(key) + `, ` + jsonEncode(val) + `)}`)
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

	time.Sleep(10 * time.Millisecond)
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
			time.Sleep(15 * time.Millisecond)
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

func (s *Session) RealClickPosition(x, y int) error {
	err := s.WriteCommand(`{"method":"Input.dispatchMouseEvent","params":{"type":"mousePressed","x":` + strconv.Itoa(x) + `,"y":` + strconv.Itoa(y) + `,"button":"left","clickCount":1}}`)
	if err != nil {
		return err
	}
	time.Sleep(80 * time.Millisecond)
	return s.WriteCommand(`{"method":"Input.dispatchMouseEvent","params":{"type":"mouseReleased","x":` + strconv.Itoa(x) + `,"y":` + strconv.Itoa(y) + `,"button":"left","clickCount":1}}`)
}

// Active the target
func (s *Session) Active() error {
	return s.WriteCommand(`{"method":"Page.bringToFront"}`)
}
