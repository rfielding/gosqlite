package main

import (
	"archive/tar"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"

	vision "cloud.google.com/go/vision/apiv1"
	_ "github.com/mattn/go-sqlite3"
)

var docExtractor string

// Make sure to only serve up out of known subdirectories
var theFS = http.FileServer(http.Dir("."))
var theDB *sql.DB
var useVisionAPI bool

// Use this for startup panics only
func CheckErr(err error, msg string) {
	if err != nil {
		log.Printf("ERR %s", msg)
		panic(err)
	}
}

// Use these on startup so that config is logged
func Getenv(k string, defaultValue string) string {
	v := os.Getenv(k)
	if v == "" {
		v = defaultValue
	}
	log.Printf("ENV %s: %s", k, v)
	return v
}

// ie: things that FTS5 can handle directly
func IsTextFile(fName string) bool {
	if strings.HasSuffix(fName, ".txt") {
		return true
	}
	if strings.HasSuffix(fName, ".json") {
		return true
	}
	if strings.HasSuffix(fName, ".html") {
		return true
	}
	return false
}

// ie: things that Tika can handle to produce IsTextFile
func IsDoc(fName string) bool {
	if strings.HasSuffix(fName, ".doc") {
		return true
	}
	if strings.HasSuffix(fName, ".ppt") {
		return true
	}
	if strings.HasSuffix(fName, ".xls") {
		return true
	}
	if strings.HasSuffix(fName, ".docx") {
		return true
	}
	if strings.HasSuffix(fName, ".pptx") {
		return true
	}
	if strings.HasSuffix(fName, ".xlsx") {
		return true
	}
	if strings.HasSuffix(fName, ".pdf") {
		return true
	}
	// ?? a guess
	if strings.HasSuffix(fName, ".one") {
		return true
	}
	return false
}

func IsVideo(fName string) bool {
	if strings.HasSuffix(fName, ".mp4") {
		return true
	}
	return false
}

func IsImage(fName string) bool {
	if strings.HasSuffix(fName, ".jpg") {
		return true
	}
	if strings.HasSuffix(fName, ".jpeg") {
		return true
	}
	if strings.HasSuffix(fName, ".png") {
		return true
	}
	if strings.HasSuffix(fName, ".gif") {
		return true
	}
	return false
}

func AsJson(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("ERR %v", err)
		return ""
	}
	return string(b)
}

// detectLabels gets labels from the Vision API for an image at the given file path.
func detectLabels(file string) (io.Reader, error) {
	ctx := context.Background()

	client, err := vision.NewImageAnnotatorClient(ctx)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	image, err := vision.NewImageFromReader(f)
	if err != nil {
		return nil, err
	}
	annotations, err := client.DetectLabels(ctx, image, nil, 10)
	if err != nil {
		return nil, err
	}

	pipeReader, pipeWriter := io.Pipe()
	go func() {
		pipeWriter.Write([]byte(AsJson(annotations)))
		pipeWriter.Close()
	}()
	return pipeReader, nil
}

func makeThumbnail(file string) (io.Reader, error) {
	command := []string{
		"convert",
		"-thumbnail", "x100",
		"-background", "white",
		"-alpha", "remove",
		"-format", "png",
		file,
		"-",
	}
	cmd := exec.Command(command[0], command[1:]...)
	// This returns an io.ReadCloser, and I don't know if it is mandatory for client to close it
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Unable to run thumbnail command: %v\n%s", err, AsJson(command))
	}
	// Give back a pipe that closes itself when it's read.
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		pipeWriter.Write(stdout)
		pipeWriter.Close()
	}()
	return pipeReader, nil
}

func pdfThumbnail(file string) (io.Reader, error) {
	command := []string{
		"convert",
		"-resize", "x100",
		file + "[0]",
		"png:-",
	}
	cmd := exec.Command(command[0], command[1:]...)
	// This returns an io.ReadCloser, and I don't know if it is mandatory for client to close it
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Unable to run thumbnail command: %v\n%s", err, AsJson(command))
	}
	// Give back a pipe that closes itself when it's read.
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		pipeWriter.Write(stdout)
		pipeWriter.Close()
	}()
	return pipeReader, nil
}

func videoThumbnail(file string) (io.Reader, error) {
	command := []string{
		"convert",
		"-resize", "x100",
		file + "[100]",
		"png:-",
	}
	cmd := exec.Command(command[0], command[1:]...)
	// This returns an io.ReadCloser, and I don't know if it is mandatory for client to close it
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Unable to run thumbnail command: %v\n%s", err, AsJson(command))
	}
	// Give back a pipe that closes itself when it's read.
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		pipeWriter.Write(stdout)
		pipeWriter.Close()
	}()
	return pipeReader, nil
}

// Make a request to tika in this case
func DocExtract(fName string, rdr io.Reader) (io.ReadCloser, error) {
	cl := http.Client{}
	req, err := http.NewRequest("PUT", docExtractor, rdr)
	if err != nil {
		return nil, fmt.Errorf("Unable to make request to upload file: %v", err)
	}
	req.Header.Add("accept", "text/plain")
	res, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Unable to do request to upload file %s: %v", fName, err)
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("Unable to upload %s: %d", fName, res.StatusCode)
	}
	return res.Body, nil
}

func indexTextFile(
	command string,
	path string,
	name string,
	part int,
	originalPath string,
	originalName string,
	content []byte,
) error {
	// index the file -- if we are appending, we should only incrementally index
	_, err := theDB.Exec(
		`INSERT INTO filesearch (cmd, path, name, part, original_path, original_name, content) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		command,
		path,
		name,
		part,
		originalPath,
		originalName,
		content,
	)
	if err != nil {
		return fmt.Errorf("ERR while indexing %s %s%s: %v", command, path, name, err)
	}
	return nil
}

// postFileHandler can be re-used as long as err != nil
func postFileHandler(
	w http.ResponseWriter,
	r *http.Request,
	stream io.Reader,
	command string,
	parentDir string,
	name string,
	originalParentDir string,
	originalName string,
	cascade bool,
) error {
	fullName := fmt.Sprintf("%s/%s", parentDir, name)
	//log.Printf("create %s %s", command, fullName)

	// Write the file out
	flags := os.O_WRONLY | os.O_CREATE

	// We either append content, or overwrite it entirely
	if command == "append" {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	// TODO: check permissions before allowing writes

	//log.Printf("Ensure existence of parentDir: %s", parentDir)
	err := os.MkdirAll("."+parentDir, 0777)
	if err != nil {
		return HandleReturnedError(w, err, "Could not create path for %s: %v", r.URL.Path)
	}

	existingSize := int64(0)
	s, err := os.Stat("." + fullName)
	if err == nil {
		existingSize = s.Size()
	}

	// Ensure that the file in question exists on disk.
	if true {
		f, err := os.Create("." + fullName) //, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return HandleReturnedError(w, err, "Could not create file %s: %v", r.URL.Path)
		}

		// Save the stream to a file
		sz, err := io.Copy(f, stream)
		f.Close() // strange positioning, but we must close before defer can get to it.
		if err != nil {
			return HandleReturnedError(w, err, "Could not write to file (%d bytes written) %s: %v", sz, r.URL.Path)
		}
	}

	if IsDoc(fullName) && cascade {
		// Open the file we wrote
		f, err := os.Open("." + fullName)
		if err != nil {
			return HandleReturnedError(w, err, "Could not open file for indexing %s: %v", fullName)
		}
		// Get a doc extract stream
		rdr, err := DocExtract(fullName, f)
		f.Close()
		if err != nil {
			return HandleReturnedError(w, err, "Could not extract file for indexing %s: %v", fullName)
		}
		// Write the doc extract stream like an upload
		extractName := fmt.Sprintf("%s--extract.txt", name)
		err = postFileHandler(w, r, rdr, command, parentDir, extractName, originalParentDir, originalName, cascade)
		if err != nil {
			return HandleReturnedError(w, err, "Could not write extract file for indexing %s: %v", fullName)
		}

		ext := strings.ToLower(path.Ext(fullName))
		if ext == ".pdf" {
			rdr, err := pdfThumbnail(`./` + fullName)
			if err != nil {
				return HandleReturnedError(w, err, "Could not make thumbnail for %s: %v", fullName)
			}
			// Only png works.  bug in imageMagick.  don't cascade on thumbnails
			thumbnailName := fmt.Sprintf("%s--thumbnail.png", name)
			err = postFileHandler(w, r, rdr, command, parentDir, thumbnailName, originalParentDir, originalName, false)
			if err != nil {
				return HandleReturnedError(w, err, "Could not write make thumbnail for indexing %s: %v", fullName)
			}
		}

		// open the file that we saved, and index it in the database.
		return nil
	}

	if IsVideo(fullName) && cascade {
		rdr, err := videoThumbnail(`./` + fullName)
		if err != nil {
			return HandleReturnedError(w, err, "Could not make thumbnail for %s: %v", fullName)
		}
		thumbnailName := fmt.Sprintf("%s--thumbnail.png", name)
		err = postFileHandler(w, r, rdr, command, parentDir, thumbnailName, originalParentDir, originalName, false)
		if err != nil {
			return HandleReturnedError(w, err, "Could not write make thumbnail for indexing %s: %v", fullName)
		}
		return nil
	}

	if IsImage(fullName) && cascade {
		if true {
			rdr, err := makeThumbnail(`./` + fullName)
			if err != nil {
				return HandleReturnedError(w, err, "Could not make thumbnail for %s: %v", fullName)
			}
			thumbnailName := fmt.Sprintf("%s--thumbnail.png", name)
			err = postFileHandler(w, r, rdr, command, parentDir, thumbnailName, originalParentDir, originalName, false)
			if err != nil {
				return HandleReturnedError(w, err, "Could not write make thumbnail for indexing %s: %v", fullName)
			}
		}

		if useVisionAPI {
			rdr, err := detectLabels(`./` + fullName)
			if err != nil {
				return HandleReturnedError(w, err, "Could not extract labels for %s: %v", fullName)
			}
			labelName := fmt.Sprintf("%s--labels.json", name)
			err = postFileHandler(w, r, rdr, command, parentDir, labelName, originalParentDir, originalName, cascade)
			if err != nil {
				//return HandleReturnedError(w, err, "Could not write extract file for indexing %s: %v", fullName)
				log.Printf("Could not write extract file for indexing %s: %v\n", fullName, err)
				return nil
			}
		}

		return nil
	}

	if IsTextFile(fullName) && cascade {
		// open the file that we saved, and index it in the database.
		f, err := os.Open("." + fullName)
		if err != nil {
			return HandleReturnedError(w, err, "Could not open file for indexing %s: %v", fullName)
		}
		defer f.Close()
		if existingSize > 0 {
			// we are appending, so we need to start at the end of the file
			f.Seek(existingSize, 0)
		}
		var rdr io.Reader = f
		/*
			if command == "files" {
				// this implies a truncate
				_, err := theDB.Exec(`DELETE from filesearch where path = ? and name = ? and cmd = ?`, parentDir+"/", name, command)
				if err != nil {
					log.Printf("cleaning out fulltextsearch for: %s%s %s failed: %v", parentDir+"/", name, command, err)
				}
			}
		*/
		buffer := make([]byte, 4*1024)
		part := 0
		for {
			sz, err := rdr.Read(buffer)
			if err == io.EOF {
				break
			}
			err = indexTextFile(command, parentDir+"/", name, part, originalParentDir+"/", originalName, buffer[:sz])
			if err != nil {
				log.Printf("failed indexing: %v", err)
			}
			part++
		}
		return nil
	}
	return nil
}

func postFilesHandler(w http.ResponseWriter, r *http.Request, pathTokens []string) {
	var err error
	defer r.Body.Close()

	q := r.URL.Query()
	// This is a signal that this is a tar archive
	// that we unpack to install all files at the given url
	needsInstall := q.Get("install") == "true"
	if needsInstall {
		log.Printf("install tarball to %s", r.URL.Path)
	}

	if len(pathTokens) < 2 {
		HandleError(w, err, "path needs /[command]/[url] for post to %s: %v", r.URL.Path)
		return
	}
	command := pathTokens[1]
	pathTokens[1] = "files"

	// Make sure that the path exists, and get the file name
	parentDir := strings.Join(pathTokens[:len(pathTokens)-1], "/")
	name := pathTokens[len(pathTokens)-1]

	// If err != nil, then we can't call this again.  http response has been sent
	if needsInstall == true { // we ASSUME that it's a tar
		// We install the tarball, walking it file by file
		t := tar.NewReader(r.Body)
		for {
			header, err := t.Next()
			if err == io.EOF {
				break
			}
			// Ignore directories for a moment XXX
			if header.Typeflag == tar.TypeReg {
				// I assume that header names are unqualified dir names
				tname := strings.Split(header.Name, "/") // expect . in front
				tname = tname[1:]
				tardir := path.Dir(fmt.Sprintf("%s/%s/%s", parentDir, name, strings.Join(tname, "/")))
				tarname := path.Base(header.Name)
				log.Printf("writing: %s into %s", tarname, tardir)
				err = postFileHandler(w, r, t, command, tardir, tarname, tardir, tarname, true)
				if err != nil {
					log.Printf("ERR %v", err)
					return
				}
			}
		}
	} else {
		// Just a normal single-file upload
		err = postFileHandler(w, r, r.Body, command, parentDir, name, parentDir, name, true)
		if err != nil {
			log.Printf("ERR %v", err)
			return
		}
	}
}

func postHandler(w http.ResponseWriter, r *http.Request, pathTokens []string) {
	if len(pathTokens) > 2 && pathTokens[1] == "files" {
		postFilesHandler(w, r, pathTokens)
		return
	}
	w.WriteHeader(http.StatusNotImplemented)
}

type Node struct {
	Attributes map[string]interface{} `json:"attributes,omitempty"`
	Path       string                 `json:"path,omitempty"`
	Name       string                 `json:"name"`
	IsDir      bool                   `json:"isDir"`
	Context    string                 `json:"context,omitempty"`
	Size       int64                  `json:"size,omitempty"`
}

type Listing struct {
	Attributes map[string]interface{} `json:"attributes,omitempty"`
	Children   []Node                 `json:"children"`
}

// Use the same format as the http.FileServer when given a directory
func getRootHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	inJson := q.Get("json") == "true"
	if inJson {
		w.Header().Set("Content-Type", "application/json")
		listing := Listing{
			Children: []Node{
				{Name: "files", IsDir: true},
			},
		}
		w.Write([]byte(AsJson(listing)))
	} else {
		// TODO: proper relative path calculation
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<form method="GET" action="/search">` + "\n"))
		w.Write([]byte(`<ul>` + "\n"))
		w.Write([]byte(`  <li><label for="match"><input id="match" name="match" type="text"><input type="button" value="search" name="search">` + "\n"))
		w.Write([]byte(`  <li><a href="/files/">files</a>` + "\n"))
		w.Write([]byte(`</ul>` + "\n"))
		w.Write([]byte(`</form>` + "\n"))
	}
}

func getSearchHandler(w http.ResponseWriter, r *http.Request, pathTokens []string) {
	match := r.URL.Query().Get("match")
	rows, err := theDB.Query(`
		SELECT original_path,original_name,part,highlight(filesearch,7,'<b style="background-color:yellow">','</b>') highlighted 
		from filesearch
		where filesearch match ?
	`, match)
	if err != nil {
		HandleError(w, err, "query %s: %v", match)
		return
	}

	q := r.URL.Query()
	inJson := q.Get("json") == "true"
	if inJson {
		w.Header().Set("Content-Type", "application/json")
		listing := Listing{
			Children: []Node{
				{Name: "files", IsDir: true},
			},
		}
		for rows.Next() {
			var path, name, highlighted string
			var part int
			rows.Scan(&path, &name, &part, &highlighted)
			listing.Children = append(listing.Children, Node{
				Path:    path,
				Name:    name,
				IsDir:   false,
				Context: highlighted,
			})
		}
		w.Write([]byte(AsJson(listing)))
	} else {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<ul>` + "\n"))
		for rows.Next() {
			var path, name, highlighted string
			var part int
			rows.Scan(&path, &name, &part, &highlighted)
			w.Write([]byte(
				fmt.Sprintf(`<li><a href="%s%s">%s%s [part %d]</a><br>%s`+"<br></li>", path, name, path, name, part, highlighted),
			))
		}
		w.Write([]byte(`</ul>`))
	}
}

func getAttrs(fsPath string, fName string) map[string]interface{} {
	// Get the attributes for the file if they exist
	var attrs map[string]interface{}
	attrFileName := fsPath + "/" + fName + "--attributes.json"
	if _, err := os.Stat(attrFileName); err == nil {
		jf, err := ioutil.ReadFile(attrFileName)
		if err != nil {
			log.Printf("Failed to open %s!: %v", attrFileName, err)
		} else {
			err := json.Unmarshal(jf, &attrs)
			if err != nil {
				log.Printf("Failed to parse %s!: %v", attrFileName, err)
			}
		}
	}
	if len(attrs) == 0 {
		return nil
	}
	return attrs
}

func dirHandler(w http.ResponseWriter, r *http.Request, fsPath string) {
	// Get directory names
	names, err := ioutil.ReadDir(fsPath)
	if err != nil {
		HandleError(w, err, "readdir %s: %v", fsPath)
		return
	}
	sort.Slice(names, func(i, j int) bool {
		return names[i].Name() < names[j].Name()
	})

	q := r.URL.Query()
	inJson := q.Get("json") == "true"
	if inJson {
		w.Header().Set("Content-Type", "application/json")
		listing := Listing{
			Children: []Node{},
		}
		for _, name := range names {
			fName := name.Name()
			attrs := getAttrs(fsPath, fName)
			listing.Children = append(listing.Children, Node{
				Name:       fName,
				IsDir:      name.IsDir(),
				Size:       name.Size(),
				Attributes: attrs,
			})
		}
		w.Write([]byte(AsJson(listing)))
	} else {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<ul>` + "\n"))
		var prevName string
		for _, name := range names {
			fName := name.Name()

			if strings.HasSuffix(fName, "--thumbnail.png") {
				continue
			}

			// If it's a derived file, then attach it to previous listing
			if strings.HasPrefix(fName, prevName) && strings.Contains(fName, prevName+"--") {
				w.Write([]byte((`  <br>&nbsp;&nbsp;` + "\n")))
			} else {
				w.Write([]byte(`  <br>&nbsp;<li>` + "\n"))
			}

			// Use an image in the link if we have a thumbnail
			sz := ""
			if name.IsDir() == false {
				if name.Size() > 1024*1024*1024 {
					sz = fmt.Sprintf(" (%d GB)", name.Size()/(1024*1024*1024))
				} else if name.Size() > 1024*1024 {
					sz = fmt.Sprintf(" (%d MB)", name.Size()/(1024*1024))
				} else if name.Size() > 1024 {
					sz = fmt.Sprintf(" (%d kB)", name.Size()/(1024))
				} else {
					sz = fmt.Sprintf(" (%d B)", name.Size())
				}
			}

			// Render security attributes
			attrs := getAttrs(fsPath, fName)
			if len(attrs) > 0 {
				label, labelOk := attrs["label"].(string)
				bg, bgOk := attrs["bg"].(string)
				fg, fgOk := attrs["fg"].(string)
				if labelOk && bgOk && fgOk {
					w.Write([]byte(fmt.Sprintf(`<span style="background-color: %s;color: %s">%s</span><br>`+"\n", bg, fg, label)))
				}
			}

			// Render the regular link
			w.Write([]byte(fmt.Sprintf(`<a href="%s">%s %s</a>`+"\n", fName, fName, sz)))

			// Render the thumbnail if we have one
			if _, err := os.Stat(fsPath + "/" + fName + "--thumbnail.png"); err == nil {
				w.Write([]byte(fmt.Sprintf(`<br><a href="%s--thumbnail.png"><img valign=bottom src="%s--thumbnail.png"></a>`+"\n", fName, fName)))
			}

			prevName = fName
		}
		w.Write([]byte(`</ul>` + "\n"))
	}
}

// Use the standard file serving of Go, because media behavior
// is really really complicated; and you do not want to serve it manually
// if you can help it.
func getHandler(w http.ResponseWriter, r *http.Request, pathTokens []string) {
	// preserve redirect parameters
	q := r.URL.Query().Encode()
	if q != "" {
		q = "?" + q
	}

	if r.URL.Path == "/" {
		getRootHandler(w, r)
		return
	}
	// Don't deal with directories missing slashes
	if r.URL.Path == "/files" {
		http.Redirect(w, r, r.URL.Path+"/"+q, http.StatusMovedPermanently)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/files/") {
		s, _ := os.Stat("." + r.URL.Path)
		if s != nil && s.IsDir() {
			if r.URL.Path[len(r.URL.Path)-1] != '/' {
				http.Redirect(w, r, r.URL.Path+"/"+q, http.StatusMovedPermanently)
				return
			}
			sIdx, _ := os.Stat("." + r.URL.Path + "index.html")
			if sIdx != nil && !sIdx.IsDir() {
				// Rather than redirect?
				http.ServeFile(w, r, "."+r.URL.Path+"index.html")
				return
			} else {
				dirHandler(w, r, "."+r.URL.Path)
				return
			}
		}
		// otherwise, just serve a file
		if strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Content-Type", "text/css")
		}
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "text/javascript")
		}
		if strings.HasSuffix(r.URL.Path, ".md") {
			w.Header().Set("Content-Type", "text/markdown")
		}
		theFS.ServeHTTP(w, r)
		return
	}
	// try search handler
	if r.URL.Path == "/search" || strings.HasPrefix(r.URL.Path, "/search/") {
		getSearchHandler(w, r, pathTokens)
		return
	}
	// give up
	w.WriteHeader(http.StatusNotFound)
}

func HandleError(w http.ResponseWriter, err error, mask string, args ...interface{}) {
	msg := fmt.Sprintf(mask, append(args, err.Error())...)
	log.Printf("ERR %s", msg)
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte(msg))
}

func HandleReturnedError(w http.ResponseWriter, err error, mask string, args ...interface{}) error {
	msg := fmt.Sprintf(mask, append(args, err.Error())...)
	log.Printf("ERR %s", msg)
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte(msg))
	return fmt.Errorf("%v", msg)
}

// We route on method and first segment of the path
func rootRouter(w http.ResponseWriter, r *http.Request) {
	pathTokens := strings.Split(r.URL.Path, "/")
	switch r.Method {
	case http.MethodGet:
		getHandler(w, r, pathTokens)
		return
	case http.MethodPost:
		postHandler(w, r, pathTokens)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// Setup theDB, and return a cleanup function
func dbSetup() func() {
	var err error
	dbName := Getenv("SCHEMA", "schema.db")
	log.Printf("opening database %s", dbName)
	theDB, err = sql.Open("sqlite3", dbName)
	CheckErr(err, fmt.Sprintf("Could not open %s", dbName))
	log.Printf("opened database %s", dbName)
	return func() {
		theDB.Close()
		log.Printf("closed database %s", dbName)
	}
}

// Launch a plain http server
func httpSetup() {
	bindAddr := Getenv("BIND", "0.0.0.0:9321")
	http.HandleFunc("/", rootRouter)
	log.Printf("start http at %s", bindAddr)
	log.Fatal(http.ListenAndServe(bindAddr, nil))
}

func main() {
	useVisionAPI = false
	/*  GoogleVision API isn't worth the trouble right now.  AWS Rekognition is definitely worth the trouble.  TODO.
	if s, err := os.Stat("./visionbot-secret-key.json"); err == nil && s.IsDir() == false && s.Size() > 0 {
		useVisionAPI = true
	} else {
		log.Printf("copy over ./visionbot-secret-key.json Google Vision API key to use automatic image labels")
	}
	log.Printf("Using the Google Vision API, because credentials are mounted")
	*/

	docExtractor = Getenv("DOC_EXTRACTOR", "http://localhost:9998/tika")

	// Set up the database
	dbCleanup := dbSetup()
	defer dbCleanup()

	// this hangs unti the server dies
	httpSetup()
}
