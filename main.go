package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html"
)

type EntryType int

const (
	InvalidEntry      EntryType = -1
	FileEntry                   = 0
	FolderEntry                 = 1
	FolderParentEntry           = 2
)

var hostUrl url.URL

var maxThreads int64
var threads int64 = 0

var writeUrlOnly bool
var urlFile *os.File
var urlFileMtx sync.Mutex

var wg sync.WaitGroup

func getDownloadPath(entryPath string) (string, error) {
	parts := []string{"downloads"}

	for _, str := range strings.Split(entryPath, "/") {
		str, err := url.PathUnescape(str)
		if err != nil {
			return "", err
		}

		str = strings.TrimSpace(str)
		if len(str) > 0 {
			parts = append(parts, str)
		}
	}

	return strings.Join(parts, "/"), nil
}
func getDownloadUrl(entryHost url.URL, entryPath string) string {
	entryHost.Path = entryPath
	return entryHost.String()
}

func GetFileSize(name string) (int64, error) {
	stat, err := os.Stat(name)
	if err == nil {
		return stat.Size(), nil
	}
	return 0, err
}

func getSubNodes(node *html.Node, nodeKey string) []*html.Node {
	var nodes = make([]*html.Node, 0)

	if node != nil {
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Data == nodeKey {
				nodes = append(nodes, child)
			}
		}

	}

	return nodes
}
func getAttribute(node *html.Node, attrKey string) *html.Attribute {
	if node != nil {
		for _, attribute := range node.Attr {
			if attribute.Key == attrKey {
				return &attribute
			}
		}
	}

	return nil
}

func GetEntryType(node *html.Node, entryType *EntryType) bool {
	*entryType = InvalidEntry

	if node == nil {
		return false
	}
	if node.FirstChild == nil {
		return false
	}
	if node.FirstChild.Data != "img" {
		return false
	}

	altAttr := getAttribute(node.FirstChild, "alt")
	if altAttr == nil {
		return false
	}

	switch altAttr.Val {
	case "file":
		*entryType = FileEntry
	case "folder":
		*entryType = FolderEntry
	case "folder-parent":
		*entryType = FolderParentEntry
	}

	return *entryType != InvalidEntry
}
func GetEntryPath(node *html.Node, entryPath *string) bool {
	*entryPath = ""

	if node == nil {
		return false
	}
	if node.FirstChild == nil {
		return false
	}
	if node.FirstChild.Data != "a" {
		return false
	}

	altAttr := getAttribute(node.FirstChild, "href")
	if altAttr == nil {
		return false
	}

	*entryPath = altAttr.Val

	return true
}
func GetEntryTime(node *html.Node, entryTime *time.Time) bool {
	*entryTime = time.Time{}

	if node == nil {
		return false
	}
	if node.FirstChild == nil {
		return false
	}
	data := node.FirstChild.Data
	t, err := time.Parse("2006-01-02 15:04", data)
	if err != nil {
		return false
	}

	*entryTime = t

	return true
}
func GetEntrySize(node *html.Node, entrySize *int64) bool {
	*entrySize = 0

	if node == nil {
		return false
	}
	if node.FirstChild == nil {
		return false
	}
	data := node.FirstChild.Data

	isKb := false
	if strings.HasSuffix(data, " KB") {
		data = data[:len(data)-3]
		isKb = true
	}

	i, err := strconv.ParseInt(data, 10, 64)
	if err != nil {
		return false
	}

	*entrySize = i
	if isKb {
		*entrySize *= 1000
	}

	return true
}
func ParseEntry(node *html.Node) {
	if node == nil {
		return
	}

	var entryType EntryType = InvalidEntry
	var entryPath string
	var entryTime time.Time
	var entrySize int64

	for _, tdNode := range getSubNodes(node, "td") {
		classAttr := getAttribute(tdNode, "class")
		if classAttr == nil {
			return
		}

		parseResult := false

		switch classAttr.Val {
		case "fb-i":
			parseResult = GetEntryType(tdNode, &entryType)
		case "fb-n":
			parseResult = GetEntryPath(tdNode, &entryPath)
		case "fb-d":
			parseResult = GetEntryTime(tdNode, &entryTime)
		case "fb-s":
			parseResult = GetEntrySize(tdNode, &entrySize)
		}

		if !parseResult {
			return
		}
	}

	if entryType == InvalidEntry || entryType == FolderParentEntry {
		return
	}

	if entryType == FolderEntry {
		crawlDirectoryAsync(entryPath)
	} else {
		saveContentAsync(entryPath, entrySize)
	}
}

func writeUrl(fileUrl string) {
	urlFileMtx.Lock()
	defer urlFileMtx.Unlock()

	urlFile.WriteString(fileUrl + "\n")
}
func downloadUrl(entryPath string, downloadSize int64) {
	fileName, err := getDownloadPath(entryPath)
	if err != nil {
		return
	}

	folder := filepath.Dir(fileName)
	if os.MkdirAll(folder, os.ModePerm) != nil {
		return
	}

	nThreads := atomic.LoadInt64(&threads)

	// Verify file integrity
	fileSize, err := GetFileSize(fileName)
	if err == nil {
		if fileSize == downloadSize {
			fmt.Printf("[%03d] Intact        # %s\n", nThreads, fileName)
			return
		}
		fmt.Printf("[%03d] Damaged       # %s\n", nThreads, fileName)
		os.Remove(fileName)
	}

	entryUrl := getDownloadUrl(hostUrl, entryPath)

	fmt.Printf("[%03d] Downloading   # %s\n", nThreads, entryUrl)
	resp, err := http.Get(entryUrl)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	fmt.Printf("[%03d] Saving        # %s\n", nThreads, fileName)
	file, err := os.Create(fileName)
	if err != nil {
		fmt.Printf("%s: %s\n", fileName, err.Error())
		os.Remove(fileName)
		return
	}
	defer file.Close()

	var buffer = make([]byte, 4096)

	for {
		n, err := resp.Body.Read(buffer)
		if err != nil {
			if err == io.EOF {
				return
			} else {
				fmt.Printf("[%03d] ERROR         # %s: %s\n", nThreads, fileName, err.Error())
				os.Remove(fileName)
				return
			}
		}

		if n != 4096 {
			_, err = file.Write(buffer[:n])
		} else {
			_, err = file.Write(buffer)
		}
		if err != nil {
			fmt.Printf("[%03d] ERROR         # %s: %s\n", nThreads, fileName, err.Error())
			os.Remove(fileName)
			return
		}
	}
}
func saveContent(fileUrl string, downloadSize int64) {
	writeUrl(fileUrl)
	if !writeUrlOnly {
		downloadUrl(fileUrl, downloadSize)
	}
}
func hasAttributeWithVal(node *html.Node, attrKey string, attrVal string) bool {
	if node != nil {
		for _, attribute := range node.Attr {
			if attribute.Key == attrKey && attribute.Val == attrVal {
				return true
			}
		}
	}

	return false
}
func crawlDirectory(entryPath string) {
	entryUrl := getDownloadUrl(hostUrl, entryPath)

	resp, err := http.Get(entryUrl)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	doc, err := html.Parse(resp.Body)
	if err != nil {
		return
	}

	var f func(*html.Node)
	f = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "tbody" {
			for _, trNode := range getSubNodes(node, "tr") {
				ParseEntry(trNode)
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
}
func saveContentAsync(entryPath string, optionalFileSize int64) {
	f_async := func() {
		wg.Add(1)
		defer wg.Done()
		saveContent(entryPath, optionalFileSize)
		atomic.AddInt64(&threads, -1)
	}
	nThreads := atomic.AddInt64(&threads, 1)
	if nThreads <= maxThreads {
		go f_async()
	} else {
		atomic.AddInt64(&threads, -1)
		saveContent(entryPath, optionalFileSize)
	}
}
func crawlDirectoryAsync(entryPath string) {
	f_async := func() {
		wg.Add(1)
		defer wg.Done()
		crawlDirectory(entryPath)
		atomic.AddInt64(&threads, -1)
	}
	nThreads := atomic.AddInt64(&threads, 1)
	if nThreads <= maxThreads {
		go f_async()
	} else {
		atomic.AddInt64(&threads, -1)
		crawlDirectory(entryPath)
	}
}

func printUsage() {
	println("Usage: h5ai-dl.exe [1] [2] [3]")
	println("    1: A url for a h5ai hsoted website")
	println("    2: Only save urls, and dont download files? (1, t, T, TRUE, true, True / 0, f, F, FALSE, false, False)")
	println("    3: [OPTIONAL] Amount of threads to use, defaults to CPU core count")
}

func main() {
	if len(os.Args) < 3 {
		printUsage()
		return
	}

	requestUrl, err := url.Parse(os.Args[1])
	if err != nil {
		printUsage()
		println("Error: 1st argument: " + err.Error())
		return
	}
	if requestUrl.Scheme != "http" && requestUrl.Scheme != "https" {
		printUsage()
		println("Error: 1st argument is not a http or https url!")
		return
	}

	hostUrl = *requestUrl
	hostUrl.Path = ""

	requestPath := requestUrl.Path

	writeUrlOnly, err = strconv.ParseBool(os.Args[2])
	if err != nil {
		printUsage()
		println("Error: 2nd argument: " + err.Error())
		return
	}

	if len(os.Args) > 3 {
		maxThreads, err = strconv.ParseInt(os.Args[3], 10, 64)
		if err != nil {
			printUsage()
			println("Error: 3rd argument: " + err.Error())
			return
		}
	} else {
		maxThreads = int64(runtime.NumCPU())
	}

	urlFile, err = os.Create("urls.txt")
	if err != nil {
		println("Error: Failed to create \"urls.txt\"")
		return
	}
	defer urlFile.Close()

	crawlDirectory(requestPath)
	time.Sleep(time.Second)
	wg.Wait()

	println("Done!")
}
