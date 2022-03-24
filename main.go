package main

import (
	"bytes"
	"encoding/json"
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

func writeUrl(fileUrl string) {
	urlFileMtx.Lock()
	defer urlFileMtx.Unlock()

	urlFile.WriteString(fileUrl + "\n")
}
func downloadUrl(entryPath string, downloadSize int64, modifiedTime uint64) {
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
func saveContent(fileUrl string, downloadSize int64, modifiedTime uint64) {
	writeUrl(fileUrl)
	if !writeUrlOnly {
		downloadUrl(fileUrl, downloadSize, modifiedTime)
	}
}

type FileEntry struct {
	Path string `json:"href"`
	Time uint64 `json:"time"`
	Size int64  `json:"size"`
}

func getFileIndex(entryPath string) ([]FileEntry, error) {
	request := struct {
		Action string `json:"action"`
		Items  struct {
			Path  string `json:"href"`
			Depth int    `json:"what"`
		} `json:"items"`
	}{}

	request.Action = "get"
	request.Items.Path = entryPath
	request.Items.Depth = 1

	request_data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(hostUrl.String(), "application/json", bytes.NewBuffer(request_data))
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return nil, err
	}
	defer resp.Body.Close()

	fileIndex := struct {
		Items []FileEntry `json:"items"`
	}{}

	err = json.NewDecoder(resp.Body).Decode(&fileIndex)
	if err != nil {
		return nil, err
	}

	return fileIndex.Items, nil
}
func crawlDirectory(entryPath string) {
	fileEntries, err := getFileIndex(entryPath)
	if err != nil {
		return
	}

	for _, entry := range fileEntries {
		if entryPath == entry.Path || !strings.HasPrefix(entry.Path, entryPath) {
			continue
		}

		if strings.HasSuffix(entry.Path, "/") {
			crawlDirectoryAsync(entry.Path)
		} else {
			saveContentAsync(entry.Path, entry.Size, entry.Time)
		}
	}
}
func saveContentAsync(entryPath string, fileSize int64, fileTime uint64) {
	f_async := func() {
		wg.Add(1)
		defer wg.Done()
		saveContent(entryPath, fileSize, fileTime)
		atomic.AddInt64(&threads, -1)
	}
	nThreads := atomic.AddInt64(&threads, 1)
	if nThreads <= maxThreads {
		go f_async()
	} else {
		atomic.AddInt64(&threads, -1)
		saveContent(entryPath, fileSize, fileTime)
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
