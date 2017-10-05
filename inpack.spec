[project]
name = sysinner-httplb
version = 0.0.3
vendor = sysinner.com
homepage = http://www.sysinner.com
groups = dev/sys-srv
description = SysInner HTTP Load Balancer

%build

mkdir -p {{.buildroot}}/bin

time go build -ldflags "-w -s" -o {{.buildroot}}/bin/sysinner-httplb ./main.go

%files
README.md
LICENSE

