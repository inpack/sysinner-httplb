// Copyright 2017 Authors, All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"fmt"
	mrand "math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hooto/hlog4g/hlog"
	"github.com/lessos/lessgo/types"
	"golang.org/x/crypto/acme/autocert"

	"github.com/sysinner/incore/inconf"
)

type PodEntry struct {
	Domain string           `json:"domain"`
	Routes []*PodEntryRoute `json:"routes"`
	Status string           `json:"status"`
}

type PodEntryRoute struct {
	Type string     `json:"type"`
	Path string     `json:"path"`
	Urls []*url.URL `json:"urls"`
}

var (
	errBody404 = []byte(`<html>
<head><title>404 Not Found</title></head>
<body bgcolor="white">
  <center><h1>404 Not Found</h1></center>
  <hr><center>InnerStack PaaS Engine</center>
</body>
</html>
`)
)

var (
	tlsCacheDir = "/opt/sysinner/httplb/var/tls_cache"

	pgPodCfr       *inconf.PodConfigurator
	tlsDomainSet   = []string{}
	tlsDomainCache = []string{}
	tlsConfSets    = map[string]string{}
	httpServer     *http.Server
	httpsServer    *http.Server

	podm sync.RWMutex
	pods = map[string]*PodEntry{}

	certManager autocert.Manager

	version = "0.11"
	release = "0"
)

func init() {
	mrand.Seed(time.Now().UnixNano())
}

type compressWriter struct {
	http.ResponseWriter
	gzipWriter *gzip.Writer
	buf        *bytes.Buffer
	statusCode int
}

func (w *compressWriter) Write(b []byte) (int, error) {

	if w.gzipWriter == nil &&
		w.Header().Get("Content-Encoding") == "gzip" {
		return w.ResponseWriter.Write(b)
	}

	if w.buf == nil {
		w.buf = &bytes.Buffer{}
	}

	if w.gzipWriter == nil {
		w.gzipWriter = gzip.NewWriter(w.buf)
	}

	return w.gzipWriter.Write(b)
}

func (w *compressWriter) WriteHeader(statusCode int) {
	if statusCode > w.statusCode {
		w.statusCode = statusCode
	}
}

func cmpHandler(fn http.HandlerFunc) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {

		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			fn(w, r)
			return
		}

		cw := &compressWriter{
			ResponseWriter: w,
		}

		fn(cw, r)

		if cw.gzipWriter != nil {
			cw.gzipWriter.Flush()
			cw.gzipWriter.Close()
			w.Header().Set("Content-Encoding", "gzip")
		}

		if cw.buf != nil && cw.buf.Len() > 0 {
			w.Header().Set("Content-Length", strconv.Itoa(cw.buf.Len()))
			if cw.statusCode > 0 {
				w.WriteHeader(cw.statusCode)
			}
			w.Write(cw.buf.Bytes())
		} else if uri := w.Header().Get("Location"); uri != "" &&
			w.Header().Get("Content-Type") == "" {
			if cw.statusCode >= 300 && cw.statusCode < 310 {
				w.WriteHeader(cw.statusCode)
			} else {
				w.WriteHeader(http.StatusFound)
			}
		} else if cw.statusCode > 0 {
			w.WriteHeader(cw.statusCode)
		}
	}
}

func httpHandler(w http.ResponseWriter, r *http.Request) {

	w.Header().Set("X-Proxy", "InnerStack/"+version)

	if pod := getPod(r.Host); pod != nil {
		//
		urlpath := filepath.Clean(r.URL.Path)
		if runtime.GOOS == "windows" {
			urlpath = strings.Replace(urlpath, "\\", "/", -1)
		}
		// urlpath = strings.Trim(urlpath, "/")

		for _, route := range pod.Routes {

			if !strings.HasPrefix(urlpath, route.Path) {
				continue
			}

			switch route.Type {

			case "pod", "upstream":
				for _, u := range route.Urls {
					p := httputil.NewSingleHostReverseProxy(u)
					p.ServeHTTP(w, r)
					return
				}

			case "redirect":
				w.Header().Set("Location", route.Urls[0].String())
				w.WriteHeader(http.StatusFound)
				return
			}
		}
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write(errBody404)
	w.WriteHeader(404)
}

func main() {

	mux := http.NewServeMux()
	mux.HandleFunc("/", cmpHandler(httpHandler))

	os.MkdirAll(tlsCacheDir, 0750)

	certManager = autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(tlsCacheDir),
		HostPolicy: autocert.HostWhitelist(tlsDomainSet...),
	}

	httpServer = &http.Server{
		Addr:    ":8080",
		Handler: certManager.HTTPHandler(nil),
	}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil {
			hlog.Printf("error", "http-server start failed : %s", err.Error())
		}
	}()

	httpsServer = &http.Server{
		Addr:    ":8443",
		Handler: mux,
		TLSConfig: &tls.Config{
			GetCertificate: certManager.GetCertificate,
		},
	}
	go func() {
		if err := httpsServer.ListenAndServeTLS("", ""); err != nil {
			hlog.Printf("error", "https-server start failed : %s", err.Error())
		}
		tlsDomainCache = nil
	}()

	for {
		time.Sleep(3e9)
		configRefresh()
	}
}

func getPod(domain string) *PodEntry {
	podm.RLock()
	defer podm.RUnlock()
	pod, ok := pods[domain]
	if ok {
		return pod
	}
	return nil
}

func configRefresh() {

	podm.Lock()
	defer podm.Unlock()

	var (
		podCfr *inconf.PodConfigurator
		appCfr *inconf.AppConfigurator
		err    error
	)

	{
		if pgPodCfr != nil {
			podCfr = pgPodCfr

			if !podCfr.Update() {
				return
			}

		} else {

			if podCfr, err = inconf.NewPodConfigurator(); err != nil {
				hlog.Print("error", err.Error())
				return
			}
		}

		appCfr = podCfr.AppConfigurator("sysinner-httplb")
		if appCfr == nil {
			hlog.Print("error", "No AppSpec (sysinner-httplb) Found")
			return
		}
	}

	tlsDomainSet = []string{}

	// hlog.Printf("info", "App Options %d", len(appCfr.App.Operate.Options))

	for _, res := range appCfr.App.Operate.Options {

		if !strings.HasPrefix(string(res.Name), "res/domain/") {
			continue
		}

		//
		var (
			domain         = strings.ToLower(string(res.Name)[len("res/domain/"):])
			optLetsEncrypt = false
			podEntry       = &PodEntry{
				Domain: domain,
			}
		)

		// location
		for _, bound := range res.Items {

			if strings.HasPrefix(bound.Name, "option/letsencrypt_enable") {
				if bound.Value == "on" {
					optLetsEncrypt = true
				}
				continue
			}

			if !strings.HasPrefix(bound.Name, "domain/basepath") {
				continue
			}

			location := bound.Name[len("domain/basepath"):]
			if location == "" {
				location = "/"
			}

			vpi := strings.Index(bound.Value, ":")
			if vpi < 2 {
				continue
			}

			var (
				bvtype  = bound.Value[:vpi]
				bvvalue = bound.Value[vpi+1:]
			)

			switch bvtype {
			case "pod":
				vs := strings.Split(bvvalue, ":")
				if len(vs) != 2 {
					continue
				}

				port, err := strconv.Atoi(vs[1])
				if err != nil {
					continue
				}

				var urls []*url.URL
				for _, v := range podCfr.Pod.Operate.BindServices {

					if v.PodId == "" || v.PodId != vs[0] {
						continue
					}

					if v.Port != uint32(port) {
						continue
					}

					for _, vh := range v.Endpoints {
						urls = append(urls, &url.URL{
							Scheme: "http",
							Host:   fmt.Sprintf("%s:%d", vh.Ip, vh.Port),
						})
					}

					break
				}

				if len(urls) > 0 {
					podEntry.Routes = append(podEntry.Routes, &PodEntryRoute{
						Path: location,
						Type: bvtype,
						Urls: urls,
					})
				}

			case "upstream":
				var (
					ups  = strings.Split(bvvalue, ";")
					urls []*url.URL
				)
				for _, upv := range ups {

					upvs := strings.Split(upv, ":")
					if len(upvs) != 2 {
						continue
					}

					if ip := net.ParseIP(upvs[0]); ip == nil || ip.To4() == nil {
						continue
					}
					upport, err := strconv.Atoi(upvs[1])
					if err != nil || upport < 1 || upport > 65505 {
						continue
					}

					urls = append(urls, &url.URL{
						Scheme: "http",
						Host:   fmt.Sprintf("%s:%d", upvs[0], upport),
					})
				}

				if len(urls) > 0 {
					podEntry.Routes = append(podEntry.Routes, &PodEntryRoute{
						Path: location,
						Type: bvtype,
						Urls: urls,
					})
				}

			case "redirect":
				if bvvalue == "" {
					continue
				}
				uri, err := url.ParseRequestURI(bvvalue)
				if err != nil {
					continue
				}
				uri.Path = filepath.Clean(uri.Path)
				if uri.Path == "" || uri.Path == "." {
					uri.Path = "/"
				} else if uri.Path[0] != 'h' && uri.Path[0] != '/' {
					continue
				}

				podEntry.Routes = append(podEntry.Routes, &PodEntryRoute{
					Path: location,
					Type: bvtype,
					Urls: []*url.URL{uri},
				})
			}
		}

		//
		if len(podEntry.Routes) == 0 {
			continue
		}

		// locations
		sort.Slice(podEntry.Routes, func(i, j int) bool {
			return strings.Compare(podEntry.Routes[i].Path, podEntry.Routes[j].Path) > 0
		})

		if optLetsEncrypt {
			tlsDomainSet, _ = types.ArrayStringSet(tlsDomainSet, domain)
		}

		pods[domain] = podEntry
	}

	pgPodCfr = podCfr

	if len(tlsDomainSet) > 0 &&
		types.ArrayStringHit(tlsDomainCache, tlsDomainSet) != len(tlsDomainSet) {
		//
		certManager.HostPolicy = autocert.HostWhitelist(tlsDomainSet...)
		tlsDomainCache = tlsDomainSet
		hlog.Printf("info", "tls refresh %d, domains %s",
			len(tlsDomainSet), strings.Join(tlsDomainSet, ","))
	}
}
