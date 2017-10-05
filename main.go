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
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lessos/lessgo/encoding/json"
	"github.com/lessos/lessgo/types"
	"github.com/sysinner/incore/inapi"
)

var (
	pod_inst      = "/home/action/.sysinner/pod_instance.json"
	ngx_bin_path  = "/home/action/apps/openresty/bin/nginx"
	ngx_pidfile   = "/home/action/apps/openresty/var/run.openresty.pid"
	ngx_conf_file = "/home/action/apps/openresty/conf/conf.d/%s.conf"

	pod_inst_updated time.Time
	ngx_upstream_tpl = `
upstream %s {
%s
}`
	ngx_location_tpl = `
    location %s {
        proxy_pass          http://%s;
        proxy_set_header    Host             $host;
        proxy_set_header    X-Real-IP        $remote_addr;
        proxy_set_header    X-Forwarded-For  $proxy_add_x_forwarded_for;
    }`
	ngx_location_redirect_http_tpl = `
    location %s {
        rewrite ^/(.*)$ %s permanent;
    }`
	ngx_location_redirect_path_tpl = `
    location %s {
		rewrite ^/(.*)$ $scheme://$host%s permanent;
    }`
	ngx_server_tpl = `
server {
    listen      %d;
    server_name %s;

    client_max_body_size 64M;
%s

%s
}
`
)

func main() {

	for {
		time.Sleep(3e9)
		do()
	}
}

func do() {

	fpbin, err := os.Open(ngx_bin_path)
	if err != nil {
		return
	}
	fpbin.Close()

	//
	pidout, err := exec.Command("pgrep", "-f", ngx_bin_path).Output()
	pid, _ := strconv.Atoi(strings.TrimSpace(string(pidout)))
	if err != nil || pid == 0 {

		if _, err = exec.Command(ngx_bin_path).Output(); err != nil {
			fmt.Println(err)
		} else {
			fmt.Println("http started")
		}
		return
	}

	start := time.Now()

	fp, err := os.Open(pod_inst)
	if err != nil {
		return
	}
	defer fp.Close()

	st, err := fp.Stat()
	if err != nil {
		return
	}
	if !st.ModTime().After(pod_inst_updated) {
		return
	}
	//
	bs, err := ioutil.ReadAll(fp)
	if err != nil {
		return
	}

	var inst inapi.Pod
	if err := json.Decode(bs, &inst); err != nil {
		return
	}

	var (
		proc_reload = false
		nss         = map[string]inapi.NsPodServiceMap{}
	)

	for _, app := range inst.Apps {

		if app.Spec.Meta.Name != "sysinner-httplb" {
			continue
		}

		for _, res := range app.Operate.Options {

			if !strings.HasPrefix(string(res.Name), "res/domain/") {
				continue
			}

			//
			var (
				domain    = string(res.Name)[len("res/domain/"):]
				upstreams = types.KvPairs{}
				locations = types.KvPairs{}
			)

			// location
			for _, bound := range res.Items {

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

					nsz, ok := nss[vs[0]]
					if !ok {

						if err := json.DecodeFile("/dev/shm/sysinner/nsz/"+vs[0], &nsz); err != nil {
							continue
						}

						nss[vs[0]] = nsz
					}

					if nsz.User == "" {
						continue
					}

					// upstreams
					var bups []string
					for _, v := range nsz.Services {

						if v.Port != uint16(port) {
							continue
						}

						for _, vh := range v.Items {
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

			ngx_conf := "# generated by sysinner\n"
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
					locs = append(locs, fmt.Sprintf(ngx_location_redirect_http_tpl, v.Key, v.Value[len("redirect:"):]))
				} else if strings.HasPrefix(v.Value, "redirect:") {
					locs = append(locs, fmt.Sprintf(ngx_location_redirect_path_tpl, v.Key, v.Value[len("redirect:"):]))
				} else {
					locs = append(locs, fmt.Sprintf(ngx_location_tpl, v.Key, v.Value))
				}
			}

			for _, sp := range inst.Operate.Replica.Ports {

				if sp.Name != "http" && sp.Name != "https" {
					continue
				}

				ngx_conf += fmt.Sprintf(
					ngx_server_tpl,
					sp.BoxPort,
					domain,
					"", // TODO https
					strings.Join(locs, "\n"),
				)
			}

			fpconf, err := os.OpenFile(fmt.Sprintf(ngx_conf_file, domain), os.O_RDWR|os.O_CREATE, 0644)
			if err != nil {
				fmt.Println(err)
				continue
			}
			defer fpconf.Close()

			fpconf.Seek(0, 0)
			fpconf.Truncate(0)

			_, err = fpconf.WriteString(ngx_conf)
			if err != nil {
				fmt.Println(err)
			}

			proc_reload = true
			fmt.Println("conf done")
		}
	}

	if proc_reload {

		if _, err := exec.Command("kill", "-s", "HUP", strconv.Itoa(pid)).Output(); err != nil {
			fmt.Println(err)
			return
		}

		fmt.Println("http reloaded")
	}

	pod_inst_updated = time.Now()

	fmt.Println("time", time.Since(start))
}
