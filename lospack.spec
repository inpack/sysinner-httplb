project.name = los-httplb
project.version = 0.0.2
project.vendor = lessos.com
project.homepage = http://www.lessos.com
project.groups = dev/sys-srv
project.description = lessOS HTTP Load Balancer

%build

mkdir -p {{.buildroot}}/bin

time go build -ldflags "-w -s" -o {{.buildroot}}/bin/los-httplbd ./main.go

%files
README.md
