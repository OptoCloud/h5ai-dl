package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

var baseUrl string

var threads int64 = 0

var writeUrlOnly bool
var urlFile *os.File
var urlFileMtx sync.Mutex

var wg sync.WaitGroup

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

	if !strings.HasSuffix(data, " KB") {
		return false
	}
	data = data[:len(data)-3]

	i, err := strconv.ParseInt(data, 10, 64)
	if err != nil {
		return false
	}

	*entrySize = i

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

	if entryPath[0] == '/' {
		entryPath = entryPath[1:]
	}

	entryUrl := baseUrl + entryPath
	if entryType == FolderEntry {
		crawlDirectoryAsync(entryUrl)
	} else {
		saveContentAsync(entryUrl, entrySize)
	}
}

func writeUrl(fileUrl string) {
	urlFileMtx.Lock()
	defer urlFileMtx.Unlock()

	urlFile.WriteString(fileUrl + "\n")
}
func downloadUrl(fileUrl string, downloadSize int64) {
	fileName, err := url.QueryUnescape(fileUrl[25:])
	if err != nil {
		return
	}

	fileName = "downloads/" + fileName

	folder := filepath.Dir(fileName)
	if os.MkdirAll(folder, os.ModePerm) != nil {
		return
	}

	// Verify file integrity
	fileSize, err := GetFileSize(fileName)
	if err == nil {
		if fileSize == downloadSize {
			fmt.Printf("Intact      ### %s\n", fileName)
			return
		}
		fmt.Printf("Damaged     ### %s\n", fileName)
		os.Remove(fileName)
	}

	fmt.Printf("Downloading ### %s\n", fileUrl)
	resp, err := http.Get(fileUrl)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}

	fmt.Printf("Saving      ### %s\n", fileName)
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
		if err == io.EOF {
			return
		} else if err != nil {
			fmt.Printf("%s: %s\n", fileName, err.Error())
			os.Remove(fileName)
			return
		}

		if n != 4096 {
			_, err = file.Write(buffer[:n])
		} else {
			_, err = file.Write(buffer)
		}
		if err != nil {
			fmt.Printf("%s: %s\n", fileName, err.Error())
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
func crawlDirectory(url string) {
	resp, err := http.Get(url)
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
func saveContentAsync(url string, optionalFileSize int64) {
	f_async := func() {
		wg.Add(1)
		defer wg.Done()
		saveContent(url, optionalFileSize)
		atomic.AddInt64(&threads, -1)
	}
	nThreads := atomic.AddInt64(&threads, 1)
	if nThreads <= 12 {
		fmt.Printf("Launching thread %v\n", nThreads)

		go f_async()
	} else {
		atomic.AddInt64(&threads, -1)
		saveContent(url, optionalFileSize)
	}
}
func crawlDirectoryAsync(url string) {
	f_async := func() {
		wg.Add(1)
		defer wg.Done()
		crawlDirectory(url)
		atomic.AddInt64(&threads, -1)
	}
	nThreads := atomic.AddInt64(&threads, 1)
	if nThreads <= 12 {
		fmt.Printf("Launching thread %v\n", nThreads)
		go f_async()
	} else {
		atomic.AddInt64(&threads, -1)
		crawlDirectory(url)
	}
}

func printUsage() {
	println("Usage: h5ai-dl.exe [1] [2]")
	println("    1: A url for a h5ai hsoted website")
	println("    2: Only save urls, and dont download files? (1, t, T, TRUE, true, True / 0, f, F, FALSE, false, False)")
}

func main() {
	var err error

	if len(os.Args) != 3 {
		printUsage()
		return
	}

	baseUrl = os.Args[1]
	if baseUrl[len(baseUrl)-1] != '/' {
		baseUrl = baseUrl + "/"
	}

	writeUrlOnly, err = strconv.ParseBool(os.Args[2])
	if err != nil {
		printUsage()
		println("Error: 2nd argument: " + err.Error())
		return
	}

	urlFile, err = os.Create("urls.txt")
	if err != nil {
		println("Error: Failed to create \"urls.txt\"")
		return
	}
	defer urlFile.Close()

	crawlDirectory(baseUrl)
	time.Sleep(time.Second)
	wg.Wait()

	println("Done!")
}
