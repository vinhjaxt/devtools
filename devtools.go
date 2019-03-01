package devtools

import (
	"bytes"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/fasthttp/websocket"
	"github.com/valyala/fasthttp"
)

// DevtoolsConn hold browser connection
type DevtoolsConn struct {
	nextSendID uint64
	Conn       *websocket.Conn
	ConnMu     *sync.Mutex
	IsClosed   atomic.Value
	Url        string
	fEvents    map[uint32]func(*gjson.Result, error)
	feMu       *sync.RWMutex
	feCount    uint32
}

var fasthttpClient = &fasthttp.Client{
	ReadTimeout:         time.Duration(10) * time.Second,
	MaxConnsPerHost:     233,
	MaxIdleConnDuration: time.Duration(600) * time.Second,
	Dial: func(addr string) (net.Conn, error) {
		return fasthttp.DialDualStackTimeout(addr, time.Second*time.Duration(10))
	},
	TLSConfig: &tls.Config{
		InsecureSkipVerify: true, // test server certificate is not trusted.
	},
}

// NewDevtools by url. Eg: http://localhost:9222
func NewDevtools(url string) (*DevtoolsConn, error) {
	req := fasthttp.AcquireRequest()
	req.SetRequestURI(url + "/json/version")
	req.Header.SetUserAgent("fasthttp/1.0.0")
	req.Header.Set("Accept", "*/*")
	resp := fasthttp.AcquireResponse()
	err := fasthttpClient.DoTimeout(req, resp, 5*time.Second)
	fasthttp.ReleaseRequest(req)
	if err != nil {
		fasthttp.ReleaseResponse(resp)
		return nil, err
	}
	wsURL := gjson.GetBytes(resp.Body(), "webSocketDebuggerUrl")
	fasthttp.ReleaseResponse(resp)
	if wsURL.Exists() == false {
		return nil, errors.New("No websocket url exist")
	}

	c, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		return nil, err
	}

	var isClosed atomic.Value
	isClosed.Store(false)
	dv := &DevtoolsConn{
		nextSendID: 0,
		Url:        url,
		Conn:       c,
		ConnMu:     &sync.Mutex{},
		IsClosed:   isClosed,
		fEvents:    map[uint32]func(*gjson.Result, error){},
		feMu:       &sync.RWMutex{},
		feCount:    0,
	}

	// err = dv.WriteCommand(`{"method":"Page.enable"}`)
	// if err != nil {
	// 	return nil, err
	// }

	go func() {
		for {
			_, body, err := c.ReadMessage()
			json := gjson.ParseBytes(body)
			go dv.broadcastDevtools(&json, err)
			if err != nil {
				isClosed.Store(true)
				c.Close()
				break
			}
		}
	}()

	return dv, nil
}

// Close close connection to browser
func (dv *DevtoolsConn) Close() error {
	dv.IsClosed.Store(true)
	err := dv.Conn.Close()
	dv.fEvents = nil
	dv.Conn = nil
	dv.feMu = nil
	return err
}

func (dv *DevtoolsConn) broadcastDevtools(body *gjson.Result, err error) {
	dv.feMu.RLock()
	for _, fn := range dv.fEvents {
		go fn(body, err)
	}
	dv.feMu.RUnlock()
}

func (dv *DevtoolsConn) addEvent(fn func(body *gjson.Result, err error)) uint32 {
	feID := atomic.AddUint32(&dv.feCount, 1)
	dv.feMu.Lock()
	dv.fEvents[feID] = fn
	dv.feMu.Unlock()
	return feID
}

func (dv *DevtoolsConn) delEvent(fID uint32) {
	dv.feMu.Lock()
	for k := range dv.fEvents {
		if k == fID {
			delete(dv.fEvents, k)
		}
	}
	dv.feMu.Unlock()
}

// SendCommand and wait for response
func (dv *DevtoolsConn) SendCommand(json string) (*gjson.Result, error) {
	if dv.IsClosed.Load().(bool) {
		return nil, errors.New("Websocket is closed")
	}

	sentID := atomic.AddUint64(&dv.nextSendID, 1)
	json, err := sjson.Set(json, "id", sentID)
	if err != nil {
		return nil, err
	}
	dv.ConnMu.Lock()
	err = dv.Conn.WriteMessage(websocket.TextMessage, []byte(json))
	dv.ConnMu.Unlock()
	if err != nil {
		dv.IsClosed.Store(true)
		dv.Conn.Close()
		return nil, err
	}

	success := make(chan *gjson.Result) // It's OK to leave this chan open. GC'll collect it
	defer dv.delEvent(dv.addEvent(func(body *gjson.Result, err error) {
		if body.Get("id").Uint() == sentID {
			success <- body
		}
	}))

	select {
	case result := <-success:
		return result, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("Timeout response")
	}
}

// WriteCommand dont wait for response
func (dv *DevtoolsConn) WriteCommand(json string) error {
	if dv.IsClosed.Load().(bool) {
		return errors.New("Websocket is closed")
	}
	sentID := atomic.AddUint64(&dv.nextSendID, 1)
	json, err := sjson.Set(json, "id", sentID)
	if err != nil {
		return err
	}
	dv.ConnMu.Lock()
	err = dv.Conn.WriteMessage(websocket.TextMessage, []byte(json))
	dv.ConnMu.Unlock()
	if err != nil {
		dv.IsClosed.Store(true)
		dv.Conn.Close()
		return err
	}
	return nil
}

// OpenTab equal OpenSession: Open existed target by id
func (dv *DevtoolsConn) OpenTab(tabID string) (*Session, error) {
	return dv.OpenSession(tabID)
}

// NewTab create new tab and open it
func (dv *DevtoolsConn) NewTab(url string) (*Session, error) {
	tab, err := dv.newTab(url)
	if err != nil {
		return nil, err
	}
	tabID := tab.Get("id")
	if tabID.Exists() {
		return dv.OpenSession(tabID.String())
	}
	return nil, errors.New("Can not create new tab")
}

func (dv *DevtoolsConn) newTab(link string) (*gjson.Result, error) {
	// http://localhost:9222/json/new?chrome://newtab
	req := fasthttp.AcquireRequest()
	req.SetRequestURI(dv.Url + "/json/new?" + url.QueryEscape(link))
	resp := fasthttp.AcquireResponse()
	err := fasthttpClient.DoTimeout(req, resp, 5*time.Second)
	fasthttp.ReleaseRequest(req)
	if err != nil {
		fasthttp.ReleaseResponse(resp)
		return nil, err
	}
	tabs := gjson.ParseBytes(resp.Body())
	fasthttp.ReleaseResponse(resp)
	return &tabs, nil
}

// NewSession create new private tab (separate context)
func (dv *DevtoolsConn) NewSession(link string) (*Session, error) {
	json, err := dv.SendCommand(`{"method":"Target.createBrowserContext"}`)
	if err != nil {
		return nil, err
	}
	bcID := json.Get("result.browserContextId")
	if bcID.Exists() == false {
		return nil, errors.New("Can not create browser context")
	}

	body, err := sjson.Set(`{"method":"Target.createTarget","params":{"url":"","browserContextId":"`+bcID.String()+`"}}`, "params.url", link)
	if err != nil {
		return nil, err
	}
	json, err = dv.SendCommand(body)
	targetID := json.Get("result.targetId")
	if !targetID.Exists() {
		return nil, errors.New("Can not create targetId by browser context")
	}
	return dv.OpenSession(targetID.String())
}

// CloseTab by id
func (dv *DevtoolsConn) CloseTab(tabID string) error {
	req := fasthttp.AcquireRequest()
	req.SetRequestURI(dv.Url + "/json/close/" + url.PathEscape(tabID))
	resp := fasthttp.AcquireResponse()
	err := fasthttpClient.DoTimeout(req, resp, 5*time.Second)
	fasthttp.ReleaseRequest(req)
	if err != nil {
		fasthttp.ReleaseResponse(resp)
		return err
	}
	body := resp.Body()
	fasthttp.ReleaseResponse(resp)
	if bytes.Equal(body, []byte("Target is closing")) {
		return nil
	}
	return errors.New(string(body))
}

// GetAllTabs of browser
func (dv *DevtoolsConn) GetAllTabs() (*gjson.Result, error) {
	// http://localhost:9222/json
	req := fasthttp.AcquireRequest()
	req.SetRequestURI(dv.Url + "/json")
	resp := fasthttp.AcquireResponse()
	err := fasthttpClient.DoTimeout(req, resp, 5*time.Second)
	fasthttp.ReleaseRequest(req)
	if err != nil {
		fasthttp.ReleaseResponse(resp)
		return nil, err
	}
	tabs := gjson.ParseBytes(resp.Body())
	fasthttp.ReleaseResponse(resp)

	if !tabs.IsArray() {
		return nil, errors.New("Can not get array of tabs")
	}
	return &tabs, nil
}

// CloseAllTabs of browser
func (dv *DevtoolsConn) CloseAllTabs() []error {
	// close all tab
	tabs, err := dv.GetAllTabs()
	if err != nil {
		return []error{err}
	}
	_, err = dv.newTab("about:blank")
	if err != nil {
		return []error{err}
	}

	time.Sleep(100 * time.Millisecond)

	errs := []error{}
	for _, tab := range tabs.Array() {
		err = dv.CloseTab(tab.Get("id").String())
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// GetExtensionSession open session of background page
func (dv *DevtoolsConn) GetExtensionSession(extID string) (*Session, error) {
	tabs, err := dv.GetAllTabs()
	if err != nil {
		return nil, err
	}
	for _, tab := range tabs.Array() {
		if tab.Get("type").String() == "background_page" {
			link, err := url.Parse(tab.Get("url").String())
			if err != nil {
				log.Println(err)
				continue
			}
			if link.Hostname() != extID {
				continue
			}
			ss, err := dv.OpenTab(tab.Get("id").String())
			if err != nil {
				return nil, err
			}
			return ss, nil
		}
	}
	return nil, errors.New("No extension background found")
}
