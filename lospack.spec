project.name = los-httplb-keeper
project.version = 0.0.3
project.vendor = lessos.com
project.homepage = http://www.lessos.com
project.groups = dev/sys-srv
project.description = lessOS HTTP Load Balancer

%build

mkdir -p {{.buildroot}}/bin

time go build -ldflags "-w -s" -o {{.buildroot}}/bin/los-httplb-keeper ./main.go

%files
README.md
