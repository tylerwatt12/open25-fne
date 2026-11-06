#!/bin/sh
# Install the four standard units, or one named unit using existing configuration.
# Other service names are never changed, stopped or restarted here.
set -eu
usage() {
    printf '%s\n' 'Usage: install-services.sh [--start]' \
        '       install-services.sh --role system|zone|site|application --unit NAME.service --config /absolute/service.json [--start]' >&2
    exit 2
}
framework_role= framework_unit= framework_config= framework_start=false framework_named=false
while [ "$#" -gt 0 ]; do
    case "$1" in
        --role|--unit|--config)
            [ "$#" -ge 2 ] || usage
            framework_named=true
            case "$1" in
                --role) framework_role=$2 ;;
                --unit) framework_unit=$2 ;;
                --config) framework_config=$2 ;;
            esac
            shift 2 ;;
        --start) framework_start=true; shift ;;
        *) usage ;;
    esac
done
framework_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && printf '%s/' "$PWD")
framework_dir=${framework_dir%/}
units_dir="${XDG_CONFIG_HOME:-"$HOME/.config"}/systemd/user"
if "$framework_named"; then
    case "$framework_role" in system|zone|site|application) ;; *) usage ;; esac
    case "$framework_unit" in
        ''|*[!a-zA-Z0-9_.:-]*) usage ;;
    esac
    case "$framework_unit" in [a-zA-Z0-9]*.service) ;; *) usage ;; esac
    [ "${#framework_unit}" -le 255 ] || usage
    case "$framework_config" in /*) ;; *) usage ;; esac
    # systemd executable paths cannot contain control characters.
    if [ "$(printf '%s' "$framework_dir" | LC_ALL=C tr -d '[:cntrl:]')" != "$framework_dir" ]; then
        printf '%s\n' 'The package directory cannot contain control characters.' >&2
        exit 2
    fi
    # ExecStart unquotes C escapes and expands specifiers. The ':' executable
    # prefix disables environment expansion, preserving literal dollar signs.
    quote_exec() {
        printf '%s\n' "$1" | awk '
            BEGIN { printf "\"" }
            NR > 1 { printf "\\n" }
            { for (i = 1; i <= length($0); i++) {
                c = substr($0, i, 1)
                if (c == "\\") printf "\\\\"
                else if (c == "\"") printf "\\\""
                else if (c == "%") printf "%%%%"
                else if (c == "\r") printf "\\r"
                else if (c == "\t") printf "\\t"
                else printf "%s", c
            } }
            END { printf "\"" }'
    }
    # WorkingDirectory is a raw path, not an ExecStart word. A final slash
    # preserves a directory ending in whitespace or a backslash.
    framework_workdir=$(printf '%s/' "$framework_dir" | sed 's/%/%%/g')
    framework_command=$(quote_exec ":$framework_dir/open25-fne")
    framework_config_arg=$(quote_exec "$framework_config")
    mkdir -p "$units_dir"
    framework_tmp=$(mktemp "$units_dir/.open25-unit.XXXXXX")
    trap 'rm -f "$framework_tmp"' EXIT
    trap 'exit 1' HUP INT TERM
    cat > "$framework_tmp" <<EOF
[Unit]
Description=Open25 FNE - $framework_role ($framework_unit)

[Service]
Type=simple
WorkingDirectory=$framework_workdir
ExecStart=$framework_command $framework_role --config $framework_config_arg
Restart=on-failure
RestartSec=2
KillMode=control-group
TimeoutStopSec=15
NoNewPrivileges=true
UMask=0077

[Install]
WantedBy=default.target
EOF
    install -m 0644 "$framework_tmp" "$units_dir/$framework_unit"
    rm -f "$framework_tmp"
    trap - EXIT HUP INT TERM
    set -- "$framework_unit"
else
    mkdir -p "$units_dir"
    set -- open25-fne-system.service open25-fne-zone.service \
        open25-fne-site.service open25-fne-application.service
    for framework_unit do
        install -m 0644 "$framework_dir/deploy/framework/$framework_unit" "$units_dir/"
    done
fi
systemctl --user daemon-reload
for framework_unit do
    systemctl --user enable "$framework_unit"
done
if "$framework_start"; then
    for framework_unit do
        systemctl --user start "$framework_unit"
    done
fi
