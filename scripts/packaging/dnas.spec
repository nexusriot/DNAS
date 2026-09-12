# RPM spec for DNAS. Built by scripts/build-rpm.sh, which fills in the version
# and hands rpmbuild a tarball of this tree.
#
# It is deliberately a near-copy of what the .deb installs, so the two packages
# put the same files in the same places and a bug report does not depend on which
# one someone used.

%global debug_package %{nil}
%global _build_id_links none

Name:           dnas
Version:        %{dnas_version}
Release:        1%{?dist}
Summary:        Definitely Not A Scam - a toy proof-of-work cryptocurrency

License:        MIT
URL:            https://github.com/nexusriot/DNAS
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  golang >= 1.21
Requires(pre):  shadow-utils
Recommends:     python3, python3-qt6

%description
A small but working proof-of-work cryptocurrency: Ed25519-signed transactions,
mining rewards with coinbase maturity, an account+nonce ledger, most-work
consensus with reorgs, an authenticated and encrypted peer-to-peer network, an
HTTP API with a web explorer, plus terminal (dnas-tui) and desktop (dnas-gui)
clients.

This is a learning project, not money. Do not point it at the internet.

%prep
%setup -q

%build
export CGO_ENABLED=0
go build -trimpath -ldflags "-s -w -X main.version=%{version}" -o dnas ./cmd/dnas
(cd tui && go build -trimpath -ldflags "-s -w -X main.version=%{version}" -o ../dnas-tui .)

%install
install -D -m 0755 dnas        %{buildroot}%{_bindir}/dnas
install -D -m 0755 dnas-tui    %{buildroot}%{_bindir}/dnas-tui
install -D -m 0644 gui/dnas_gui.py %{buildroot}%{_datadir}/dnas/dnas_gui.py
install -D -m 0755 /dev/stdin  %{buildroot}%{_bindir}/dnas-gui <<'LAUNCHER'
#!/bin/sh
exec python3 /usr/share/dnas/dnas_gui.py "$@"
LAUNCHER
install -D -m 0644 scripts/packaging/dnas.service %{buildroot}%{_unitdir}/dnas.service
install -D -m 0644 README.md   %{buildroot}%{_docdir}/%{name}/README.md
install -D -m 0644 QUICKSTART.md %{buildroot}%{_docdir}/%{name}/QUICKSTART.md
install -D -m 0644 scripts/monitoring/grafana-dashboard.json \
    %{buildroot}%{_datadir}/dnas/monitoring/grafana-dashboard.json
install -D -m 0644 scripts/monitoring/prometheus-alerts.yml \
    %{buildroot}%{_datadir}/dnas/monitoring/prometheus-alerts.yml
install -d -m 0750 %{buildroot}%{_sharedstatedir}/dnas
install -d -m 0750 %{buildroot}%{_sysconfdir}/dnas

%pre
getent group dnas >/dev/null || groupadd --system dnas
getent passwd dnas >/dev/null || \
    useradd --system --gid dnas --home-dir %{_sharedstatedir}/dnas \
            --shell /sbin/nologin --comment "DNAS node" dnas
exit 0

%post
%systemd_post dnas.service

%preun
%systemd_preun dnas.service

%postun
%systemd_postun_with_restart dnas.service

%files
%{_bindir}/dnas
%{_bindir}/dnas-tui
%{_bindir}/dnas-gui
%{_datadir}/dnas/dnas_gui.py
%{_datadir}/dnas/monitoring/grafana-dashboard.json
%{_datadir}/dnas/monitoring/prometheus-alerts.yml
%{_unitdir}/dnas.service
%doc %{_docdir}/%{name}/README.md
%doc %{_docdir}/%{name}/QUICKSTART.md
%attr(0750, dnas, dnas) %dir %{_sharedstatedir}/dnas
%attr(0750, root, dnas) %dir %{_sysconfdir}/dnas

%changelog
* Fri Sep 12 2026 DNAS <vananyev@hystax.com> - %{dnas_version}-1
- Packaged from the tree; see CHANGELOG.md for what changed.
