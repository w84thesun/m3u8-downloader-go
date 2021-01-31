package main

import (
	"bytes"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafov/m3u8"
	"github.com/greyh4t/hackpool"
	"github.com/guonaihong/clop"

	"m3u8-downloader-go/decrypter"
	"m3u8-downloader-go/joiner"
	"m3u8-downloader-go/zhttp"
)

var (
	ZHTTP        *zhttp.Zhttp
	conf         *Conf
	keyCache     = map[string][]byte{}
	keyCacheLock sync.Mutex
	headers      map[string]string
)

type Conf struct {
	URL       string        `clop:"-u; --url" usage:"url of m3u8 file"`
	File      string        `clop:"-f; --m3u8-file" usage:"local m3u8 file"`
	ThreadNum int           `clop:"-n; --thread-number" usage:"thread number" default:"10"`
	OutFile   string        `clop:"-o; --out-file" usage:"out file"`
	Retry     int           `clop:"-r; --retry" usage:"number of retries" default:"3"`
	Timeout   time.Duration `clop:"-t; --timeout" usage:"timeout" default:"30s"`
	Proxy     string        `clop:"-p; --proxy" usage:"proxy. Example: http://127.0.0.1:8080"`
	Headers   []string      `clop:"-H; --header; greedy" usage:"http header. Example: Referer:http://www.example.com"`
	InFile    string        `clop:"-i; --in-file" usage:"input file with URLs"`
	headers   map[string]string
}

func main() {
	conf = &Conf{}
	clop.CommandLine.SetExit(true)
	clop.Bind(&conf)

	checkConf()

	if len(conf.Headers) > 0 {
		conf.headers = map[string]string{}
		for _, header := range conf.Headers {
			s := strings.SplitN(header, ":", 2)
			key := strings.TrimRight(s[0], " ")
			if len(s) == 2 {
				conf.headers[key] = strings.TrimLeft(s[1], " ")
			} else {
				conf.headers[key] = ""
			}
		}
	}

	var err error
	ZHTTP, err = zhttp.New(conf.Timeout, conf.Proxy)
	if err != nil {
		log.Fatalln("[-] Init failed:", err)
	}

	if conf.InFile != "" {
		m3u8Files, err := processInFile(conf.InFile)
		if err != nil {
			log.Fatalln("[-] Failed to process input file:", err)
		}

		for name, mediaURL := range m3u8Files {
			m3u8File, err := downloadM3u8(mediaURL)
			if err != nil {
				log.Fatalln("[-] Download m3u8 file failed:", err)
			}

			mpl, err := parseM3u8(m3u8File)
			if err != nil {
				log.Fatalln("[-] Parse m3u8 file failed:", err)
			} else {
				log.Println("[+] Parse m3u8 file succeed")
			}

			downloadFile(mpl, name)
		}

		return
	}

	var m3u8File []byte
	if conf.File != "" {
		m3u8File, err = ioutil.ReadFile(conf.File)
		if err != nil {
			log.Fatalln("[-] Load m3u8 file failed:", err)
		}
	} else {
		m3u8File, err = downloadM3u8(conf.URL)
		if err != nil {
			log.Fatalln("[-] Download m3u8 file failed:", err)
		}
	}

	mpl, err := parseM3u8(m3u8File)
	if err != nil {
		log.Fatalln("[-] Parse m3u8 file failed:", err)
	} else {
		log.Println("[+] Parse m3u8 file succeed")
	}

	outFile := conf.OutFile
	if outFile == "" {
		outFile = filename(mpl.Segments[0].URI)

	}

	downloadFile(mpl, outFile)
}

func processInFile(file string) (map[string]string, error) {
	inFile, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	csvFile := csv.NewReader(bytes.NewReader(inFile))

	records, err := csvFile.ReadAll()
	if err != nil {
		return nil, err
	}

	urls := make(map[string]string)
	for _, record := range records {
		urls[record[0]] = record[1]
	}

	return urls, nil
}

func downloadFile(mpl *m3u8.MediaPlaylist, outFile string) {
	joiner, err := joiner.New(outFile)
	if err != nil {
		log.Fatalln("[-] Open file failed:", err)
	} else {
		log.Println("[+] Will save to", joiner.Name())
	}

	if mpl.Count() > 0 {
		log.Println("[+] Total", mpl.Count(), "files to download")

		start(joiner, mpl)

		err = joiner.Run(int(mpl.Count()))
		if err != nil {
			log.Fatalln("[-] Write to file failed:", err)
		}
		log.Println("[+] Download succeed, saved to", joiner.Name())
	}
}

func checkConf() {
	if conf.URL == "" && conf.File == "" && conf.InFile == "" {
		fmt.Println("You must set the -u or -f parameter")
		clop.Usage()
	}

	if conf.ThreadNum <= 0 {
		conf.ThreadNum = 10
	}

	if conf.Retry <= 0 {
		conf.Retry = 1
	}

	if conf.Timeout <= 0 {
		conf.Timeout = time.Second * 30
	}
}

func start(joiner *joiner.Joiner, mpl *m3u8.MediaPlaylist) {
	pool := hackpool.New(conf.ThreadNum, download)

	go func() {
		var count = int(mpl.Count())
		for i := 0; i < count; i++ {
			pool.Push(i, mpl.Segments[i], mpl.Key, joiner)
		}
		pool.CloseQueue()
	}()

	go pool.Run()
}

func downloadM3u8(m3u8URL string) ([]byte, error) {
	statusCode, data, err := ZHTTP.Get(m3u8URL, conf.headers, conf.Retry)
	if err != nil {
		return nil, err
	}

	if statusCode/100 != 2 || len(data) == 0 {
		return nil, errors.New("http code: " + strconv.Itoa(statusCode))
	}

	return data, nil
}

func parseM3u8(data []byte) (*m3u8.MediaPlaylist, error) {
	playlist, listType, err := m3u8.Decode(*bytes.NewBuffer(data), true)
	if err != nil {
		return nil, err
	}

	if listType == m3u8.MEDIA {
		var obj *url.URL
		if conf.URL != "" {
			obj, err = url.Parse(conf.URL)
			if err != nil {
				return nil, errors.New("parse m3u8 url failed: " + err.Error())
			}
		}

		mpl := playlist.(*m3u8.MediaPlaylist)

		if mpl.Key != nil && mpl.Key.URI != "" {
			uri, err := formatURI(obj, mpl.Key.URI)
			if err != nil {
				return nil, err
			}
			mpl.Key.URI = uri
		}

		count := int(mpl.Count())
		for i := 0; i < count; i++ {
			segment := mpl.Segments[i]

			uri, err := formatURI(obj, segment.URI)
			if err != nil {
				return nil, err
			}
			segment.URI = uri

			if segment.Key != nil && segment.Key.URI != "" {
				uri, err := formatURI(obj, segment.Key.URI)
				if err != nil {
					return nil, err
				}
				segment.Key.URI = uri
			}

			mpl.Segments[i] = segment
		}

		return mpl, nil
	}

	return nil, errors.New("unsupported m3u8 type")
}

func getKey(url string) ([]byte, error) {
	keyCacheLock.Lock()
	defer keyCacheLock.Unlock()

	key := keyCache[url]
	if key != nil {
		return key, nil
	}

	statusCode, key, err := ZHTTP.Get(url, headers, conf.Retry)
	if err != nil {
		return nil, err
	}

	if statusCode/100 != 2 || len(key) == 0 {
		return nil, errors.New("http code: " + strconv.Itoa(statusCode))
	}

	keyCache[url] = key

	return key, nil
}

func download(args ...interface{}) {
	id := args[0].(int)
	segment := args[1].(*m3u8.MediaSegment)
	globalKey := args[2].(*m3u8.Key)
	joiner := args[3].(*joiner.Joiner)

	statusCode, data, err := ZHTTP.Get(segment.URI, headers, conf.Retry)
	if err != nil {
		log.Fatalln("[-] Download failed:", id, err)
	}

	if statusCode/100 != 2 || len(data) == 0 {
		log.Fatalln("[-] Download failed, http code:", statusCode)
	}

	var keyURL, ivStr string
	if segment.Key != nil && segment.Key.URI != "" {
		keyURL = segment.Key.URI
		ivStr = segment.Key.IV
	} else if globalKey != nil && globalKey.URI != "" {
		keyURL = globalKey.URI
		ivStr = globalKey.IV
	}

	if keyURL != "" {
		var key, iv []byte
		key, err = getKey(keyURL)
		if err != nil {
			log.Fatalln("[-] Download key failed:", keyURL, err)
		}

		if ivStr != "" {
			iv, err = hex.DecodeString(strings.TrimPrefix(ivStr, "0x"))
			if err != nil {
				log.Fatalln("[-] Decode iv failed:", err)
			}
		} else {
			iv = []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(id)}
		}

		data, err = decrypter.Decrypt(data, key, iv)
		if err != nil {
			log.Fatalln("[-] Decrypt failed:", err)
		}
	}

	log.Println("[+] Download succeed:", id, segment.URI)

	joiner.Join(id, data)
}

func formatURI(base *url.URL, u string) (string, error) {
	if strings.HasPrefix(u, "http") {
		return u, nil
	}

	if base == nil {
		return "", errors.New("you must set m3u8 url for " + conf.File + " to download")
	}

	obj, err := base.Parse(u)
	if err != nil {
		return "", err
	}

	return obj.String(), nil
}

func filename(u string) string {
	obj, _ := url.Parse(u)
	_, filename := filepath.Split(obj.Path)
	return filename
}
