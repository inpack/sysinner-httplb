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
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hooto/hlog4g/hlog"
	"github.com/lessos/lessgo/types"
	"github.com/sysinner/incore/inconf"
	"golang.org/x/crypto/acme/autocert"
)

var (
	ngx_bin_path      = "/opt/openresty/openresty/bin/nginx"
	ngx_pidfile       = "/opt/openresty/openresty/var/run.openresty.pid"
	ngx_conf_dir      = "/opt/openresty/openresty/conf/conf.d"
	ngx_conf_file     = ngx_conf_dir + "/%s.conf"
	ngx_conf_tls_file = ngx_conf_dir + "/%s.tls.conf"
	tlsCacheDir       = "/opt/openresty/openresty/var/tls_cache"

	ngx_upstream_tpl = `
upstream %s {
%s
}`
	ngx_location_tpl = `
    location %s {
        proxy_pass          %s://%s;
        proxy_set_header    Host             $host;
        proxy_set_header    X-Real-IP        $remote_addr;
        proxy_set_header    X-Forwarded-For  $proxy_add_x_forwarded_for;
        proxy_http_version  1.1;
        proxy_set_header    Upgrade          $http_upgrade;
        proxy_set_header    Connection       $connection_upgrade;
    }`
	ngx_location_redirect_http_tpl = `
    location %s {
        rewrite ^%s(.*)$ %s$1 permanent;
    }`
	ngx_location_redirect_path_tpl = `
    location %s {
        rewrite ^%s(.*)$ $scheme://$host%s$1 permanent;
    }`
	ngx_server_tpl = `
server {
    listen      %s;
    server_name %s;

    client_max_body_size 64M;
%s

%s
}
`
	pgPodCfr       *inconf.PodConfigurator
	configs        = map[string]string{}
	tlsDomainSet   = []string{}
	tlsDomainConf  = []string{}
	tlsDomainCache = []string{}
	tlsConfSets    = map[string]string{}
	tlsServer      *http.Server
	tlsServerTLS   *http.Server
	tlsUpstreamUrl = &url.URL{Scheme: "http", Host: "127.0.0.1:8080"}
)

func main() {

	os.MkdirAll(tlsCacheDir, 0755)

	for {

		time.Sleep(3e9)

		httpRefresh()

		httpsRefresh()
	}
}

func httpsRefresh() error {

	if len(tlsDomainSet) == 0 {
		return nil
	}

	if false && types.ArrayStringHit(tlsDomainConf, tlsDomainSet) != len(tlsDomainSet) {

		for _, v := range tlsDomainSet {

			_, err := os.Stat(tlsCacheDir + "/" + v)
			if err != nil {
				continue
			}

			cfg, ok := tlsConfSets[v]
			if !ok {
				continue
			}

			err = fileSync(fmt.Sprintf(ngx_conf_tls_file, v), cfg)
			if err != nil {
				continue
			}

			tlsDomainConf, _ = types.ArrayStringSet(tlsDomainConf, v)
		}
	}

	if types.ArrayStringHit(tlsDomainCache, tlsDomainSet) != len(tlsDomainSet) {

		if tlsServer != nil {
			tlsServer.Close()
		}

		if tlsServerTLS != nil {
			tlsServerTLS.Close()
		}

		time.Sleep(1e9)

		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

			/**
			r.Proto = "HTTP/1.1"
			r.ProtoMajor = 1
			r.ProtoMinor = 1
			*/

			p := httputil.NewSingleHostReverseProxy(tlsUpstreamUrl)
			p.ServeHTTP(w, r)
		})

		certManager := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(tlsCacheDir),
			HostPolicy: autocert.HostWhitelist(tlsDomainSet...),
		}

		tlsServer = &http.Server{
			Addr:    ":1080",
			Handler: certManager.HTTPHandler(nil),
		}
		go func() {
			if err := tlsServer.ListenAndServe(); err != nil {
				hlog.Printf("error", "tls refresh failed : %s", err.Error())
			}
		}()

		tlsDomainCache = tlsDomainSet

		tlsServerTLS = &http.Server{
			Addr:    ":8443",
			Handler: mux,
			TLSConfig: &tls.Config{
				GetCertificate: certManager.GetCertificate,
			},
		}
		go func() {
			if err := tlsServerTLS.ListenAndServeTLS("", ""); err != nil {
				hlog.Printf("error", "tls refresh failed : %s", err.Error())
			}
			tlsDomainCache = nil
		}()

		hlog.Printf("info", "tls refresh %d, domains %s",
			len(tlsDomainSet), strings.Join(tlsDomainSet, ","))
	}

	return nil
}

func httpRefresh() {

	if _, err := os.Stat(ngx_bin_path); err != nil {
		return
	}

	//
	pidout, err := exec.Command("pgrep", "-f", ngx_bin_path).Output()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(pidout)))
	if err != nil || pid == 0 {

		if _, err = exec.Command(ngx_bin_path).Output(); err != nil {
			hlog.Printf("error", "setup err %s", err.Error())
		} else {
			hlog.Printf("info", "server started")
		}
		return
	}

	var (
		tstart = time.Now()
		podCfr *inconf.PodConfigurator
		appCfr *inconf.AppConfigurator
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

	var (
		procReload = false
	)

	tlsDomainSet = []string{}

	// hlog.Printf("info", "App Options %d", len(appCfr.App.Operate.Options))

	for _, res := range appCfr.App.Operate.Options {

		if !strings.HasPrefix(string(res.Name), "res/domain/") {
			continue
		}

		//
		var (
			domain         = string(res.Name)[len("res/domain/"):]
			upstreams      = types.KvPairs{}
			locations      = types.KvPairs{}
			optLetsencrypt = false
		)

		// location
		for _, bound := range res.Items {

			if strings.HasPrefix(bound.Name, "option/letsencrypt_enable") {
				if bound.Value == "on" {
					optLetsencrypt = true
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

				// upstreams
				var bups []string
				for _, v := range podCfr.Pod.Operate.BindServices {

					if v.PodId == "" || v.PodId != vs[0] {
						continue
					}

					if v.Port != uint32(port) {
						continue
					}

					for _, vh := range v.Endpoints {
						bups = append(bups, fmt.Sprintf("    server %s:%d weight=1 max_fails=2 fail_timeout=10s;", vh.Ip, vh.Port))
					}

					break
				}

				if len(bups) > 0 {
					upsname := fmt.Sprintf("sysinner_nsz_%s_%s_%d",
						domain,
						vs[0],
						port,
					)
					upstreams.Set(upsname, strings.Join(bups, "\n"))
					locations.Set(location, upsname)
				}

			case "upstream":
				ups := strings.Split(bvvalue, ";")
				var bups []string
				for _, upv := range ups {

					upvs := strings.Split(upv, ":")
					if len(upvs) != 2 {
						continue
					}

					if ip := net.ParseIP(upvs[0]); ip == nil || ip.To4() == nil {
						continue
					}
					upport, err := strconv.Atoi(upvs[1])
					if err != nil || upport < 80 || upport > 65505 {
						continue
					}

					bups = append(bups, fmt.Sprintf("    server %s:%d weight=1 max_fails=2 fail_timeout=10s;", upvs[0], upport))
				}

				if len(bups) > 0 {
					upsname := fmt.Sprintf("sysinner_ups_%s_%s",
						domain,
						strings.Replace(location, "/", "_", -1),
					)
					upstreams.Set(upsname, strings.Join(bups, "\n"))
					locations.Set(location, upsname)
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

				locations.Set(location, "redirect:"+uri.String())
			}
		}

		//
		if len(upstreams) == 0 && len(locations) == 0 {
			continue
		}

		var (
			ups  = []string{}
			locs = []string{}
		)

		ngx_conf := "# generated by sysinner http load balancer\n"
		ngx_conf += "# DO NOT EDIT!\n"

		// upstreams
		if len(upstreams) > 0 {

			sort.Slice(upstreams, func(i, j int) bool {
				return upstreams[i].Key > upstreams[j].Key
			})
			for _, v := range upstreams {
				ups = append(ups, fmt.Sprintf(ngx_upstream_tpl, v.Key, v.Value))
			}

			ngx_conf += strings.Join(ups, "\n")
			ngx_conf += "\n"
		}

		// locations
		sort.Slice(locations, func(i, j int) bool {
			return locations[i].Key > locations[j].Key
		})
		for _, v := range locations {
			if strings.HasPrefix(v.Value, "redirect:http") {
				locs = append(locs, fmt.Sprintf(ngx_location_redirect_http_tpl,
					v.Key, v.Key, v.Value[len("redirect:"):]))
			} else if strings.HasPrefix(v.Value, "redirect:") {
				locs = append(locs, fmt.Sprintf(ngx_location_redirect_path_tpl,
					v.Key, v.Key, v.Value[len("redirect:"):]))
			} else {
				locs = append(locs, fmt.Sprintf(ngx_location_tpl, v.Key, "http", v.Value))
			}
		}

		for _, sp := range podCfr.Pod.Replica.Ports {

			if sp.Name != "http" && sp.Name != "https" {
				continue
			}

			tlsCfg := ""
			listen := fmt.Sprintf("%d", sp.BoxPort)

			if optLetsencrypt {
				if sp.Name == "http" {
					locs = append([]string{fmt.Sprintf(ngx_location_tpl, "/.well-known", "http", "127.0.0.1:1080")}, locs...)
					// locs = []string{fmt.Sprintf(ngx_location_tpl, "/", "http", "127.0.0.1:1080")}
				} else if sp.Name == "https" {

					// locs = append([]string{fmt.Sprintf(ngx_location_tpl, "/", "127.0.0.1:1443")}, locs...)
					// locs = []string{fmt.Sprintf(ngx_location_tpl, "/", "https", "127.0.0.1:1443")}

					// listen += " ssl http2"
					// listen += " ssl"

					/**
					tlsCfg += fmt.Sprintf("    ssl on;\n")
					tlsCfg += fmt.Sprintf("    ssl_certificate     %s/%s;\n", tlsCacheDir, domain)
					tlsCfg += fmt.Sprintf("    ssl_certificate_key %s/%s;\n", tlsCacheDir, domain)
					tlsCfg += fmt.Sprintf("    ssl_protocols       TLSv1 TLSv1.1 TLSv1.2 TLSv1.3;\n")
					tlsCfg += fmt.Sprintf("    ssl_ciphers         HIGH:!aNULL:!MD5;\n")
					*/
				}
			}

			cfg := fmt.Sprintf(
				ngx_server_tpl,
				listen,
				domain,
				tlsCfg,
				strings.Join(locs, "\n"),
			)

			if sp.Name == "https" {
				tlsConfSets[domain] = cfg
			} else {
				ngx_conf += cfg
			}
		}

		if pv, ok := configs[domain]; ok && pv == ngx_conf {
			continue
		}

		if err := fileSync(fmt.Sprintf(ngx_conf_file, domain), ngx_conf); err != nil {
			hlog.Printf("error", "setup err %s", err.Error())
			continue
		}

		procReload = true
		configs[domain] = ngx_conf

		if optLetsencrypt {
			tlsDomainSet, _ = types.ArrayStringSet(tlsDomainSet, domain)
		}
	}

	if procReload {

		if _, err := exec.Command("kill", "-s", "HUP", strconv.Itoa(pid)).Output(); err != nil {
			hlog.Printf("info", "server reload err %s", err.Error())
			return
		}

		hlog.Printf("info", "server reload in %v", time.Since(tstart))
	}

	pgPodCfr = podCfr

	time.Sleep(1e9)
}

func fileSync(file, body string) error {

	fp, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer fp.Close()

	fp.Seek(0, 0)
	fp.Truncate(0)

	_, err = fp.WriteString(body)
	if err != nil {
		return err
	}

	return fp.Sync()
}
