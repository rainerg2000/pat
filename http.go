// Copyright 2016 Martin Hebnes Pedersen (LA5NTA). All rights reserved.
// Use of this source code is governed by the MIT-license that can be
// found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/microcosm-cc/bluemonday"

	"github.com/la5nta/pat/internal/gpsd"
	"github.com/la5nta/wl2k-go/catalog"
	"github.com/la5nta/wl2k-go/fbb"
	"github.com/la5nta/wl2k-go/mailbox"
)

// Status represents a status report as sent to the Web GUI
type Status struct {
	ActiveListeners []string `json:"active_listeners"`
	Connected       bool     `json:"connected"`
	RemoteAddr      string   `json:"remote_addr"`
	HTTPClients     []string `json:"http_clients"`
}

// Progress represents a progress report as sent to the Web GUI
type Progress struct {
	BytesTransferred int    `json:"bytes_transferred"`
	BytesTotal       int    `json:"bytes_total"`
	MID              string `json:"mid"`
	Subject          string `json:"subject"`
	Receiving        bool   `json:"receiving"`
	Sending          bool   `json:"sending"`
	Done             bool   `json:"done"`
}

// Notification represents a desktop notification as sent to the Web GUI
type Notification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Form
type Form struct {
	Name       string
	TxtFileURI string
	InitialURI string
	ViewerURI  string
	ReplyURI   string
}

// Folder with forms
type FormFolder struct {
	Name      string
	Path      string
	Version   string
	FormCount int
	Forms     []Form
	Folders   []FormFolder
}

type FormData struct {
	TargetForm Form
	Fields     map[string]string
	MsgSubject string
	MsgBody    string
	MsgXml     string
}

var postedFormData map[string]FormData

var websocketHub *WSHub

//go:generate go install -v ./vendor/github.com/jteeuwen/go-bindata/go-bindata ./vendor/github.com/elazarl/go-bindata-assetfs/go-bindata-assetfs
//go:generate go-bindata-assetfs res/...
func ListenAndServe(addr string) error {
	log.Printf("Starting HTTP service (%s)...", addr)

	postedFormData = make(map[string]FormData)

	if host, _, _ := net.SplitHostPort(addr); host == "" && config.GPSd.EnableHTTP {
		// TODO: maybe make a popup showing the warning ont the web UI?
		fmt.Fprintf(logWriter, "\nWARNING: You have enable GPSd HTTP endpoint (enable_http). You might expose"+
			"\n         your current position to anyone who has access to the Pat web interface!\n\n")
	}

	r := mux.NewRouter()
	r.HandleFunc("/api/connect_aliases", connectAliasesHandler).Methods("GET")
	r.HandleFunc("/api/connect", ConnectHandler)
	r.HandleFunc("/api/formcatalog", getFormsHandler).Methods("GET")
	r.HandleFunc("/api/form", postFormData).Methods("POST")
	r.HandleFunc("/api/form", getFormData).Methods("GET")
	r.HandleFunc("/api/forms", getFormTemplate).Methods("GET")
	r.HandleFunc("/api/mailbox/{box}", mailboxHandler).Methods("GET")
	r.HandleFunc("/api/mailbox/{box}/{mid}", messageHandler).Methods("GET")
	r.HandleFunc("/api/mailbox/{box}/{mid}", messageDeleteHandler).Methods("DELETE")
	r.HandleFunc("/api/mailbox/{box}/{mid}/{attachment}", attachmentHandler).Methods("GET")
	r.HandleFunc("/api/mailbox/{box}/{mid}/read", readHandler).Methods("POST")
	r.HandleFunc("/api/mailbox/{box}", postMessageHandler).Methods("POST")
	r.HandleFunc("/api/posreport", postPositionHandler).Methods("POST")
	r.HandleFunc("/api/status", statusHandler).Methods("GET")
	r.HandleFunc("/api/current_gps_position", positionHandler).Methods("GET")
	r.HandleFunc("/ws", wsHandler)
	r.HandleFunc("/ui", uiHandler).Methods("GET")
	r.HandleFunc("/", rootHandler).Methods("GET")

	http.Handle("/", r)
	http.Handle("/res/", http.StripPrefix("/res/", http.FileServer(assetFS())))

	websocketHub = NewWSHub()

	return http.ListenAndServe(addr, nil)
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui", http.StatusFound)
}

func connectAliasesHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.ConnectAliases)
}

func buildFormFolder(rootPath string) (FormFolder, error) {
	rootFile, err := os.Open(rootPath)
	if err != nil {
		return FormFolder{}, err
	}
	rootFileInfo, err := os.Stat(rootPath)

	if !rootFileInfo.IsDir() {
		return FormFolder{}, errors.New(rootPath + " is not a directory")
	}

	formsArr := make([]Form, 1000)
	foldersArr := make([]FormFolder, 1000)

	retVal := FormFolder{
		Name:      rootFileInfo.Name(),
		Path:      rootFile.Name(),
		FormCount: 0,
		Forms:     formsArr[0:0],
		Folders:   foldersArr[0:0],
	}

	infos, err := rootFile.Readdir(0)
	if err != nil {
		return retVal, err
	}
	rootFile.Close()

	folderCnt := 0
	formCnt := 0
	for _, info := range infos {
		if info.IsDir() {
			folderCnt++
			retVal.Folders = foldersArr[0:folderCnt]
			retVal.Folders[folderCnt-1], err = buildFormFolder(path.Join(rootPath, info.Name()))
			if err != nil {
				return retVal, err
			}
			retVal.FormCount += retVal.Folders[folderCnt-1].FormCount
		} else {
			if filepath.Ext(info.Name()) == ".txt" {
				txtURI, initialURI, viewerURI, replyURI, err := GetHtmlUrisFromFormTxt(path.Join(rootPath, info.Name()))
				if err != nil {
					continue
				}
				if initialURI != "" || viewerURI != "" {
					formCnt++
					retVal.Forms = formsArr[0:formCnt]
					retVal.Forms[formCnt-1] = Form{
						Name:       strings.TrimSuffix(filepath.Base(info.Name()), ".txt"),
						TxtFileURI: txtURI,
						InitialURI: initialURI,
						ViewerURI:  viewerURI,
						ReplyURI:   replyURI,
					}
					retVal.FormCount++
				}
			}
		}
	}
	sort.Slice(retVal.Folders, func(i, j int) bool {
		return retVal.Folders[i].Name < retVal.Folders[j].Name
	})
	sort.Slice(retVal.Forms, func(i, j int) bool {
		return retVal.Forms[i].Name < retVal.Forms[j].Name
	})
	return retVal, nil
}

func GetHtmlUrisFromFormTxt(txtPath string) (string, string, string, string, error) {
	fd, err := os.Open(txtPath)
	if err != nil {
		return "", "", "", "", err
	}
	scanner := bufio.NewScanner(fd)
	formsPathWithSlash := config.FormsPath + "/"
	txtPathTrimmed := strings.TrimPrefix(txtPath, formsPathWithSlash)
	baseURI := path.Dir(txtPathTrimmed)
	initialPath := ""
	viewerPath := ""
	replyPath := ""
	for scanner.Scan() {
		l := scanner.Text()
		if strings.HasPrefix(l, "Form:") {
			trimmed := strings.TrimSpace(strings.TrimPrefix(l, "Form:"))
			fileNamePattern := regexp.MustCompile(`[\w\s\-]+\.html`)
			fileNames := fileNamePattern.FindAllString(trimmed, -1)
			if fileNames != nil && len(fileNames) >= 2 {
				initialPath = path.Join(baseURI, fileNames[0])
				viewerPath = path.Join(baseURI, fileNames[1])
			}
		}
		if strings.HasPrefix(l, "ReplyTemplate:") {
			replyPath = strings.TrimSpace(strings.TrimPrefix(l, "ReplyTemplate:"))
			replyPath = path.Join(baseURI, replyPath)
		}
	}
	fd.Close()
	//log.Printf("'%s' '%s' '%s' '%s'", txtPathTrimmed, initialPath, viewerPath, replyPath)
	return txtPathTrimmed, initialPath, viewerPath, replyPath, nil
}

func getFormsHandler(w http.ResponseWriter, r *http.Request) {
	formFolder, err := buildFormFolder(config.FormsPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("%s %s: %s", r.Method, r.URL.Path, err)
		return
	}
	formFolder.Version = getFormsVersion(config.FormsPath)
	json.NewEncoder(w).Encode(formFolder)
}

func postFormData(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10000000); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	formPath, ok := r.URL.Query()["formPath"]
	if !ok {
		http.Error(w, "formPath query param missing", http.StatusBadRequest)
		log.Printf("formPath query param missing %s %s", r.Method, r.URL.Path)
	}

	formFolder, err := buildFormFolder(config.FormsPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("%s %s: %s", r.Method, r.URL.Path, err)
		return
	}

	form, err := findFormFromURI(formPath[0], formFolder)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("can't find form to match posted form data %s %s", formPath[0], r.URL)
		return
	}

	key, err := r.Cookie("forminstance")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("missing cookie %s %s", formPath[0], r.URL)
		return
	}
	var formData FormData
	formData.TargetForm = form
	formData.Fields = make(map[string]string)
	for key, values := range r.PostForm {
		formData.Fields[strings.ToLower(key)] = values[0]
	}

	msgSubject, msgBody, msgXml, err := buildFormMessage(form, formData.Fields, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("%s %s: %s", r.Method, r.URL.Path, err)
	}
	formData.MsgSubject = msgSubject
	formData.MsgBody = msgBody
	formData.MsgXml = msgXml
	postedFormData[key.Value] = formData
	r.Body.Close()
	io.WriteString(w, "<script>window.close()</script>")
}

func findFormFromURI(path string, folder FormFolder) (Form, error) {
	var retVal Form
	retVal.Name = "unknown"
	for _, subFolder := range folder.Folders {
		form, err := findFormFromURI(path, subFolder)
		if err == nil {
			return form, nil
		}
	}
	for _, form := range folder.Forms {
		if form.InitialURI == path || form.InitialURI == path+".html" || form.ViewerURI == path || form.ViewerURI == path+".html" || form.ReplyURI == path || form.ReplyURI == path+".0" || form.TxtFileURI == path || form.TxtFileURI == path+".txt" {
			return form, nil
		}
	}
	return retVal, errors.New("form not found")
}

func getFormData(w http.ResponseWriter, r *http.Request) {
	key, err := r.Cookie("forminstance")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("missing cookie %s %s", key, r.URL)
		return
	}
	json.NewEncoder(w).Encode(postedFormData[key.Value])
}

func readHandler(w http.ResponseWriter, r *http.Request) {
	var data struct{ Read bool }
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		log.Printf("%s %s: %s", r.Method, r.URL.Path, err)
		return
	}

	box, mid := mux.Vars(r)["box"], mux.Vars(r)["mid"]

	msg, err := mailbox.OpenMessage(path.Join(mbox.MBoxPath, box, mid+mailbox.Ext))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := mailbox.SetUnread(msg, !data.Read); err != nil {
		log.Printf("%s %s: %s", r.Method, r.URL.Path, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func postPositionHandler(w http.ResponseWriter, r *http.Request) {
	var pos catalog.PosReport

	if err := json.NewDecoder(r.Body).Decode(&pos); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body.Close()

	if pos.Date.IsZero() {
		pos.Date = time.Now()
	}

	// Post to outbox
	msg := pos.Message(fOptions.MyCall)
	if err := mbox.AddOut(msg); err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	} else {
		fmt.Fprintln(w, "Position update posted")
	}
}

func isInPath(base string, path string) error {
	_, err := filepath.Rel(base, path)
	return err
}

func postMessageHandler(w http.ResponseWriter, r *http.Request) {
	box := mux.Vars(r)["box"]
	if box == "out" {
		postOutboundMessageHandler(w, r)
		return
	}

	srcPath := r.Header.Get("X-Pat-SourcePath")
	if srcPath == "" {
		http.Error(w, "Not implemented", http.StatusNotImplemented)
		return
	}

	srcPath = strings.TrimPrefix(srcPath, "/api/mailbox/")
	srcPath = filepath.Join(mbox.MBoxPath, srcPath+mailbox.Ext)

	// Check that we don't escape our mailbox path
	srcPath = filepath.Clean(srcPath)
	if err := isInPath(mbox.MBoxPath, srcPath); err != nil {
		log.Println("Malicious source path in move:", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	targetPath := filepath.Join(mbox.MBoxPath, box, filepath.Base(srcPath))

	if err := os.Rename(srcPath, targetPath); err != nil {
		log.Println("Could not move message:", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
	} else {
		json.NewEncoder(w).Encode("OK")
	}
}

func postOutboundMessageHandler(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(10 * (1024 ^ 2)) // 10Mb
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m := r.MultipartForm

	msg := fbb.NewMessage(fbb.Private, fOptions.MyCall)

	// files
	files := m.File["files"]
	for _, f := range files {
		// For some unknown reason, we receive this empty unnamed file when no
		// attachment is provided. Prior to Go 1.10, this was filtered by
		// multipart.Reader.
		if isEmptyFormFile(f) {
			continue
		}

		if f.Filename == "" {
			http.Error(w, "Missing attachment name", http.StatusBadRequest)
			return
		}
		file, err := f.Open()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		p, err := ioutil.ReadAll(file)
		file.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if isImageMediaType(f.Filename, f.Header.Get("Content-Type")) {
			log.Printf("Auto converting '%s' [%s]...", f.Filename, f.Header.Get("Content-Type"))

			if converted, err := convertImage(bytes.NewReader(p)); err != nil {
				log.Printf("Error converting image: %s", err)
			} else {
				log.Printf("Done converting '%s'.", f.Filename)

				ext := path.Ext(f.Filename)
				f.Filename = f.Filename[:len(f.Filename)-len(ext)] + ".jpg"
				p = converted
			}
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		msg.AddFile(fbb.NewFile(f.Filename, p))
	}

	key, err := r.Cookie("forminstance")
	if err == nil {
		xml := postedFormData[key.Value].MsgXml
		form := postedFormData[key.Value].TargetForm
		msg.AddFile(fbb.NewFile(GetXmlAttachmentNameForForm(form), []byte(xml)))
	}

	// Other fields
	if v := m.Value["to"]; len(v) == 1 {
		addrs := strings.FieldsFunc(v[0], SplitFunc)
		msg.AddTo(addrs...)
	}
	if v := m.Value["cc"]; len(v) == 1 {
		addrs := strings.FieldsFunc(v[0], SplitFunc)
		msg.AddCc(addrs...)
	}
	if v := m.Value["subject"]; len(v) == 1 {
		msg.SetSubject(v[0])
	}
	if v := m.Value["body"]; len(v) == 1 {
		msg.SetBody(v[0])
	}
	if v := m.Value["p2ponly"]; len(v) == 1 && v[0] != "" {
		msg.Header.Set("X-P2POnly", "true")
	}
	if v := m.Value["date"]; len(v) == 1 {
		t, err := time.Parse(time.RFC3339, v[0])
		if err != nil {
			log.Printf("Unable to parse message date: %s", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg.SetDate(t)
	} else {
		log.Printf("Missing date value")
		http.Error(w, "Missing date value", http.StatusBadRequest)
		return
	}

	if err := msg.Validate(); err != nil {
		http.Error(w, "Validation error: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Post to outbox
	if err := mbox.AddOut(msg); err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	var buf bytes.Buffer
	msg.Write(&buf)
	fmt.Fprintf(w, "Message posted (%.2f kB)", float64(buf.Len()/1024))
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	conn.WriteJSON(struct{ MyCall string }{fOptions.MyCall})
	websocketHub.Handle(conn)
}

func uiHandler(w http.ResponseWriter, r *http.Request) {
	data, err := Asset(path.Join("res", "tmpl", "index.html"))
	if err != nil {
		log.Fatal(err)
	}

	t := template.New("index.html") //create a new template
	t, err = t.Parse(string(data))
	if err != nil {
		log.Fatal(err)
	}

	tmplData := struct{ AppName, Version, Mycall string }{AppName, versionString(), fOptions.MyCall}

	err = t.Execute(w, tmplData)
	if err != nil {
		log.Fatal(err)
	}
}

func getFormTemplate(w http.ResponseWriter, r *http.Request) {
	formPath, ok := r.URL.Query()["formPath"]
	if !ok {
		http.Error(w, "formPath query param missing", http.StatusBadRequest)
		log.Printf("formPath query param missing %s %s", r.Method, r.URL.Path)
	}

	absPathTemplate := path.Join(config.FormsPath, strings.TrimLeft(path.Clean(formPath[0]), "./\\"))

	// now deal with cases where the html file name specified in the .txt file, has differnet caseness than the actual .html file on disk.
	absPathTemplateFolder := filepath.Dir(absPathTemplate)

	templateDirFd, err := os.Open(absPathTemplateFolder)
	if err != nil {
		http.Error(w, "can't read template folder", http.StatusBadRequest)
		log.Printf("can't read template folder %s, %s", absPathTemplateFolder, r.URL.Path)
	}

	fileNames, err := templateDirFd.Readdirnames(0)
	if err != nil {
		http.Error(w, "can't read template folder", http.StatusBadRequest)
		log.Printf("can't read template folder %s, %s", absPathTemplateFolder, r.URL.Path)
	}

	templateDirFd.Close()

	absPathTemplate = ""
	for _, name := range fileNames {
		if strings.HasSuffix(strings.ToLower(formPath[0]), strings.ToLower(name)) {
			absPathTemplate = path.Join( absPathTemplateFolder, name)
			break
		}
	}

	fd, err := os.Open(absPathTemplate)
	if err != nil {
		http.Error(w, "can't open template "+formPath[0], http.StatusBadRequest)
		log.Printf("can't find form template file %s %s: %s", r.Method, r.URL.Path, "can't open template "+formPath[0])
	}

	now := time.Now()
	nowDateTime := now.Format(time.RFC3339)
	nowDateTimeUTC := now.UTC().Format(time.RFC3339)
	nowDate := now.Format("02-Jan-2006")
	nowTime := now.Format("15:04 MST")

	scanner := bufio.NewScanner(fd)
	for scanner.Scan() {
		l := scanner.Text()
		l = strings.Replace(l, "http://{FormServer}:{FormPort}", "form?"+r.URL.Query().Encode(), -1)
		// some Canada BC forms don't use the {FormServer} placeholder, it's OK, can deal with it here
		l = strings.Replace(l, "http://localhost:8001", "form?"+r.URL.Query().Encode(), -1)
		l = strings.Replace(l, "{MsgSender}", fOptions.MyCall, -1)
		l = strings.Replace(l, "{Callsign}", fOptions.MyCall, -1)
		l = strings.Replace(l, "{ProgramVersion}", "Pat " + versionStringShort(), -1)
		l = strings.Replace(l, "{DateTime}", nowDateTime, -1)
		l = strings.Replace(l, "{UDateTime}", nowDateTimeUTC, -1)
		l = strings.Replace(l, "{Date}", nowDate, -1)
		l = strings.Replace(l, "{UDTG}", nowDate, -1)
		l = strings.Replace(l, "{Time}", nowTime, -1)
		_, err = io.WriteString(w, l+"\n")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			log.Printf("can't write form template into response %s %s: %s", r.Method, r.URL.Path, err)
		}
	}
	fd.Close()
}

func getStatus() Status {
	status := Status{
		ActiveListeners: []string{},
		Connected:       exchangeConn != nil,
		HTTPClients:     websocketHub.ClientAddrs(),
	}

	for _, tl := range listenHub.Active() {
		status.ActiveListeners = append(status.ActiveListeners, tl.Name())
	}
	sort.Strings(status.ActiveListeners)

	if exchangeConn != nil {
		addr := exchangeConn.RemoteAddr()
		status.RemoteAddr = fmt.Sprintf("%s:%s", addr.Network(), addr)
	}

	return status
}

func statusHandler(w http.ResponseWriter, req *http.Request) { json.NewEncoder(w).Encode(getStatus()) }

func positionHandler(w http.ResponseWriter, req *http.Request) {
	// Throw error if GPSd http endpoint is not enabled
	if !config.GPSd.EnableHTTP || config.GPSd.Addr == "" {
		http.Error(w, "GPSd not enabled or address not set in config file", http.StatusInternalServerError)
		return
	}

	host, _, _ := net.SplitHostPort(req.RemoteAddr)
	log.Printf("Location data from GPSd served to %s", host)

	conn, err := gpsd.Dial(config.GPSd.Addr)
	if err != nil {
		// do not pass error message to response as GPSd address might be leaked
		http.Error(w, "GPSd Dial failed", http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	conn.Watch(true)

	pos, err := conn.NextPosTimeout(5 * time.Second)
	if err != nil {
		http.Error(w, "GPSd get next position failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if config.GPSd.UseServerTime {
		pos.Time = time.Now()
	}

	json.NewEncoder(w).Encode(pos)
	return
}

func ConnectHandler(w http.ResponseWriter, req *http.Request) {
	connectStr := req.FormValue("url")

	nMsgs := mbox.InboxCount()

	if success := Connect(connectStr); !success {
		http.Error(w, "Session failure", http.StatusInternalServerError)
	}

	json.NewEncoder(w).Encode(struct {
		NumReceived int
	}{
		mbox.InboxCount() - nMsgs,
	})
}

func mailboxHandler(w http.ResponseWriter, r *http.Request) {
	box := mux.Vars(r)["box"]

	var messages []*fbb.Message
	var err error

	switch box {
	case "in":
		messages, err = mbox.Inbox()
	case "out":
		messages, err = mbox.Outbox()
	case "sent":
		messages, err = mbox.Sent()
	case "archive":
		messages, err = mbox.Archive()
	default:
		http.NotFound(w, r)
		return
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		log.Println(err)
	}

	sort.Sort(sort.Reverse(fbb.ByDate(messages)))

	jsonSlice := make([]JSONMessage, len(messages))
	for i, msg := range messages {
		jsonSlice[i] = JSONMessage{Message: msg}
	}
	json.NewEncoder(w).Encode(jsonSlice)
	return
}

type JSONMessage struct {
	*fbb.Message
	inclBody bool
}

func (m JSONMessage) MarshalJSON() ([]byte, error) {

	msg := struct {
		MID      string
		Date     time.Time
		From     fbb.Address
		To       []fbb.Address
		Cc       []fbb.Address
		Subject  string
		Body     string
		BodyHTML string
		Files    []*fbb.File
		P2POnly  bool
		Unread   bool
	}{
		MID:     m.MID(),
		Date:    m.Date(),
		From:    m.From(),
		To:      m.To(),
		Cc:      m.Cc(),
		Subject: m.Subject(),
		Files:   m.Files(),
		P2POnly: m.Header.Get("X-P2POnly") == "true",
		Unread:  mailbox.IsUnread(m.Message),
	}

	if m.inclBody {
		msg.Body, _ = m.Body()
		unsafe := toHTML([]byte(msg.Body))
		msg.BodyHTML = string(bluemonday.UGCPolicy().SanitizeBytes(unsafe))
	}
	return json.Marshal(msg)
}

func messageDeleteHandler(w http.ResponseWriter, r *http.Request) {
	box, mid := mux.Vars(r)["box"], mux.Vars(r)["mid"]

	path := filepath.Clean(filepath.Join(mbox.MBoxPath, box, mid+mailbox.Ext))
	if err := isInPath(mbox.MBoxPath, path); err != nil {
		log.Println("Malicious source path in move:", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	err := os.Remove(path)
	if os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	json.NewEncoder(w).Encode("OK")
}

func messageHandler(w http.ResponseWriter, r *http.Request) {
	box, mid := mux.Vars(r)["box"], mux.Vars(r)["mid"]

	msg, err := mailbox.OpenMessage(path.Join(mbox.MBoxPath, box, mid+mailbox.Ext))
	if os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(JSONMessage{msg, true})
}

func attachmentHandler(w http.ResponseWriter, r *http.Request) {
	// Attachments are potentially unsanitized HTML and/or javascript.
	// To avoid XSS, we enable the CSP sandbox directive so that these
	// attachments can't call other parts of the API (deny same origin).
	w.Header().Set("Content-Security-Policy", "sandbox allow-forms allow-modals allow-orientation-lock allow-pointer-lock allow-popups allow-popups-to-escape-sandbox allow-presentation allow-scripts")

	// Allow different sandboxed attachments to refer to each other.
	// This can be useful to provide rich HTML content as attachments,
	// without having to bundle it all up in one big file.
	w.Header().Set("Access-Control-Allow-Origin", "null")

	box, mid, attachment := mux.Vars(r)["box"], mux.Vars(r)["mid"], mux.Vars(r)["attachment"]

	msg, err := mailbox.OpenMessage(path.Join(mbox.MBoxPath, box, mid+mailbox.Ext))
	if os.IsNotExist(err) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		log.Println(err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Find and write attachment
	var found bool
	for _, f := range msg.Files() {
		if f.Name() != attachment {
			continue
		}
		found = true
		http.ServeContent(w, r, f.Name(), msg.Date(), bytes.NewReader(f.Data()))
	}

	if !found {
		http.NotFound(w, r)
	}
}

// toHTML takes the given body and turns it into proper html with
// paragraphs, blockquote, and <br /> line breaks.
func toHTML(body []byte) []byte {
	buf := bytes.NewBuffer(body)
	var out bytes.Buffer

	fmt.Fprint(&out, "<p>")

	scanner := bufio.NewScanner(buf)

	var blockquote int
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			fmt.Fprint(&out, "</p><p>")
			continue
		}

		depth := blockquoteDepth(line)
		for depth != blockquote {
			if depth > blockquote {
				fmt.Fprintf(&out, "</p><blockquote><p>")
				blockquote++
			} else {
				fmt.Fprintf(&out, "</p></blockquote><p>")
				blockquote--
			}
		}
		line = line[depth:]

		line = htmlEncode(line)
		line = linkify(line)

		fmt.Fprint(&out, line+"\n")
	}

	for ; blockquote > 0; blockquote-- {
		fmt.Fprintf(&out, "</p></blockquote>")
	}

	fmt.Fprint(&out, "</p>")
	return out.Bytes()
}

// blcokquoteDepth counts the number of '>' at the beginning of the string.
func blockquoteDepth(str string) (n int) {
	for _, c := range str {
		if c != '>' {
			break
		}
		n++
	}
	return
}

// htmlEncode encodes html characters
func htmlEncode(str string) string {
	str = strings.Replace(str, ">", "&gt;", -1)
	str = strings.Replace(str, "<", "&lt;", -1)
	return str
}

// linkify detects url's in the given string and adds <a href tag.
//
// It is recursive.
func linkify(str string) string {
	start := strings.Index(str, "http")

	var needScheme bool
	if start < 0 {
		start = strings.Index(str, "www.")
		needScheme = true
	}

	if start < 0 {
		return str
	}

	end := strings.IndexAny(str[start:], " ,()[]")
	if end < 0 {
		end = len(str)
	} else {
		end += start
	}

	link := str[start:end]
	if needScheme {
		link = "http://" + link
	}

	return fmt.Sprintf(`%s<a href='%s' target='_blank'>%s</a>%s`, str[:start], link, str[start:end], linkify(str[end:]))
}
