package wails

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/dchest/htmlmin"
	"github.com/gobuffalo/packr"
	"github.com/gorilla/websocket"
)

var headlessAssets = packr.NewBox("./assets/headless")
var defaultAssets = packr.NewBox("./assets/default")

type messageType int

const (
	jsMessage messageType = iota
	cssMessage
	htmlMessage
	notifyMessage
	bindingMessage
	callbackMessage
	wailsRuntimeMessage
)

func (m messageType) toString() string {
	return [...]string{"j", "s", "h", "n", "b", "c", "w"}[m]
}

// Headless is a backend that opens a local web server
// and renders the files over a websocket
type Headless struct {
	// Common
	log          *CustomLogger
	ipcManager   *ipcManager
	appConfig    *AppConfig
	eventManager *eventManager
	bindingCache []string
	frameworkJS  string
	frameworkCSS string
	jsCache      []string
	cssCache     []string

	// Headless specific
	initialisationJS []string
	server           *http.Server
	theConnection    *websocket.Conn
	bridgeMode       bool
	connectionType   string
}

// Initialise the Headless Renderer
func (h *Headless) Initialise(appConfig *AppConfig, ipcManager *ipcManager, eventManager *eventManager) error {
	h.ipcManager = ipcManager
	h.appConfig = appConfig
	h.eventManager = eventManager
	h.bridgeMode = false
	h.connectionType = "Websocket"

	ipcManager.bindRenderer(h)
	h.log = newCustomLogger("Headless")
	return nil
}

func (h *Headless) evalJS(js string, mtype messageType) error {

	message := mtype.toString() + js

	if h.theConnection == nil {
		h.initialisationJS = append(h.initialisationJS, message)
	} else {
		// Prepend message type to message
		h.sendMessage(h.theConnection, message)
	}

	return nil
}

func (h *Headless) injectCSS(css string) {
	// Minify css to overcome issues in the browser with carriage returns
	minified, err := htmlmin.Minify([]byte(css), &htmlmin.Options{
		MinifyStyles: true,
	})
	if err != nil {
		h.log.Fatal("Unable to minify CSS: " + css)
	}
	minifiedCSS := string(minified)
	minifiedCSS = strings.Replace(minifiedCSS, "\\", "\\\\", -1)
	minifiedCSS = strings.Replace(minifiedCSS, "'", "\\'", -1)
	minifiedCSS = strings.Replace(minifiedCSS, "\n", " ", -1)
	inject := fmt.Sprintf("wails._.injectCSS('%s')", minifiedCSS)
	h.evalJS(inject, cssMessage)
}

func (h *Headless) rootHandler(w http.ResponseWriter, r *http.Request) {
	indexHTML := BoxString(&headlessAssets, "index.html")
	fmt.Fprintf(w, "%s", indexHTML)
}

func (h *Headless) wsBridgeHandler(w http.ResponseWriter, r *http.Request) {
	h.bridgeMode = true
	h.connectionType = "Bridge"
	h.wsHandler(w, r)
}

func (h *Headless) wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Upgrade(w, r, w.Header(), 1024, 1024)
	if err != nil {
		http.Error(w, "Could not open websocket connection", http.StatusBadRequest)
	}
	h.theConnection = conn
	h.log.Infof("%s connection accepted [%p].", h.connectionType, h.theConnection)
	conn.SetCloseHandler(func(int, string) error {
		h.log.Infof("%s connection dropped [%p].", h.connectionType, h.theConnection)
		h.theConnection = nil
		return nil
	})
	go h.start(conn)
}

func (h *Headless) sendMessage(conn *websocket.Conn, msg string) {
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		h.log.Error(err.Error())
	}
}

func (h *Headless) start(conn *websocket.Conn) {

	// set external.invoke
	h.log.Infof("Connected to frontend.")
	h.log.Infof("Mode = %s", h.connectionType)

	wailsRuntime := BoxString(&defaultAssets, "wails.js")
	h.evalJS(wailsRuntime, wailsRuntimeMessage)

	if !h.bridgeMode {
		// Inject jquery
		jquery := BoxString(&defaultAssets, "jquery.3.3.1.min.js")
		h.evalJS(jquery, jsMessage)
	}

	// Inject the initial JS
	for _, js := range h.initialisationJS {
		h.sendMessage(h.theConnection, js)
	}

	// Inject bindings
	for _, binding := range h.bindingCache {
		h.evalJS(binding, bindingMessage)
	}

	// In Bridge mode, we only send the wails runtime and bindings
	// so ignore this whole section
	if !h.bridgeMode {
		// Inject Framework
		if h.frameworkJS != "" {
			h.evalJS(h.frameworkJS, jsMessage)
		}
		if h.frameworkCSS != "" {
			h.injectCSS(h.frameworkCSS)
		}

		// Inject user CSS
		if h.appConfig.CSS != "" {
			outputCSS := fmt.Sprintf("%.45s", h.appConfig.CSS)
			if len(outputCSS) > 45 {
				outputCSS += "..."
			}
			h.log.DebugFields("Inject User CSS", Fields{"css": outputCSS})
			h.injectCSS(h.appConfig.CSS)
		} else {
			// Use default wails css
			h.log.Debug("Injecting Default Wails CSS")
			defaultCSS := BoxString(&defaultAssets, "wails.css")

			h.injectCSS(defaultCSS)
		}

		// Inject all the CSS files that have been added
		for _, css := range h.cssCache {
			h.injectCSS(css)
		}

		// Inject all the JS files that have been added
		for _, js := range h.jsCache {
			h.evalJS(js, jsMessage)
		}

		// Inject user JS
		if h.appConfig.JS != "" {
			outputJS := fmt.Sprintf("%.45s", h.appConfig.JS)
			if len(outputJS) > 45 {
				outputJS += "..."
			}
			h.log.DebugFields("Inject User JS", Fields{"js": outputJS})
			h.evalJS(h.appConfig.JS, jsMessage)
		}

		var injectHTML string
		if h.appConfig.isHTMLFragment {
			injectHTML = fmt.Sprintf("$('#app').html('%s')", h.appConfig.HTML)
			h.evalJS(injectHTML, htmlMessage)
		}
	}

	// Emit that everything is loaded and ready
	h.eventManager.Emit("wails:ready")

	for {
		messageType, buffer, err := conn.ReadMessage()
		if messageType == -1 {
			return
		}
		if err != nil {
			h.log.Errorf("Error reading message: ", err)
			continue
		}

		h.log.Debugf("Got message: %#v\n", string(buffer))

		h.ipcManager.Dispatch(string(buffer))
	}
}

// Run the app in headless mode!
func (h *Headless) Run() error {
	h.server = &http.Server{Addr: ":34115"}
	http.HandleFunc("/ws", h.wsHandler)
	http.HandleFunc("/bridge", h.wsBridgeHandler)
	http.HandleFunc("/", h.rootHandler)

	h.log.Info("Headless mode started.")
	h.log.Info("If using the Wails bridge, it will connect automatically.")
	h.log.Info("You may also connect manually by browsing to http://localhost:34115")

	err := h.server.ListenAndServe()
	if err != nil {
		h.log.Fatal(err.Error())
	}
	return err
}

// NewBinding creates a new binding with the frontend
func (h *Headless) NewBinding(methodName string) error {
	h.bindingCache = append(h.bindingCache, methodName)
	return nil
}

// InjectFramework sets up what JS/CSS should be injected
// at startup
func (h *Headless) InjectFramework(js, css string) {
	h.frameworkJS = js
	h.frameworkCSS = css
}

// SelectFile is unsupported for Headless but required
// for the Renderer interface
func (h *Headless) SelectFile() string {
	h.log.Error("SelectFile() unsupported in headless mode")
	return ""
}

// SelectDirectory is unsupported for Headless but required
// for the Renderer interface
func (h *Headless) SelectDirectory() string {
	h.log.Error("SelectDirectory() unsupported in headless mode")
	return ""
}

// SelectSaveFile is unsupported for Headless but required
// for the Renderer interface
func (h *Headless) SelectSaveFile() string {
	h.log.Error("SelectSaveFile() unsupported in headless mode")
	return ""
}

// AddJSList adds a slice of JS strings to the list of js
// files injected at startup
func (h *Headless) AddJSList(jsCache []string) {
	h.jsCache = jsCache
}

// AddCSSList adds a slice of CSS strings to the list of css
// files injected at startup
func (h *Headless) AddCSSList(cssCache []string) {
	h.cssCache = cssCache
}

// Callback sends a callback to the frontend
func (h *Headless) Callback(data string) error {
	return h.evalJS(data, callbackMessage)
}

// NotifyEvent notifies the frontend of an event
func (h *Headless) NotifyEvent(event *eventData) error {

	// Look out! Nils about!
	var err error
	if event == nil {
		err = fmt.Errorf("Sent nil event to renderer.webViewRenderer")
		logger.Error(err)
		return err
	}

	// Default data is a blank array
	data := []byte("[]")

	// Process event data
	if event.Data != nil {
		// Marshall the data
		data, err = json.Marshal(event.Data)
		if err != nil {
			h.log.Errorf("Cannot unmarshall JSON data in event: %s ", err.Error())
			return err
		}
	}

	message := fmt.Sprintf("window.wails._.notify('%s','%s')", event.Name, data)
	return h.evalJS(message, notifyMessage)
}

// SetColour is unsupported for Headless but required
// for the Renderer interface
func (h *Headless) SetColour(colour string) error {
	h.log.WarnFields("SetColour ignored for headless more", Fields{"col": colour})
	return nil
}

// Fullscreen is unsupported for Headless but required
// for the Renderer interface
func (h *Headless) Fullscreen() {
	h.log.Warn("Fullscreen() unsupported in headless mode")
}

// UnFullscreen is unsupported for Headless but required
// for the Renderer interface
func (h *Headless) UnFullscreen() {
	h.log.Warn("UnFullscreen() unsupported in headless mode")
}

// SetTitle is currently unsupported for Headless but required
// for the Renderer interface
func (h *Headless) SetTitle(title string) {
	h.log.WarnFields("SetTitle() unsupported in headless mode", Fields{"title": title})
}

// Close is unsupported for Headless but required
// for the Renderer interface
func (h *Headless) Close() {
	h.log.Warn("Close() unsupported in headless mode")
}
