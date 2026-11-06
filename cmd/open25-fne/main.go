package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"open25.local/open25-fne/internal/activity"
	"open25.local/open25-fne/internal/framework"
	"open25.local/open25-fne/internal/frameworkui"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Open25 FNE:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("use init, system, zone, site, application, enroll-site, enroll-application, or radio")
	}
	if args[0] == "init" {
		f := flag.NewFlagSet("init", flag.ContinueOnError)
		config := f.String("config", "", "private initial configuration file")
		var o framework.InitOptions
		f.StringVar(&o.Root, "root", "", "new private framework directory")
		f.StringVar(&o.ListenHost, "listen-host", "127.0.0.1", "listener host")
		f.StringVar(&o.InternalListenHost, "internal-listen-host", "127.0.0.1", "VPN service listener host")
		f.StringVar(&o.SystemInternalURL, "system-internal-url", "", "System internal HTTP origin")
		f.StringVar(&o.ZoneInternalURL, "zone-internal-url", "", "Zone internal HTTP origin")
		f.StringVar(&o.SiteInternalURL, "site-internal-url", "", "Site internal HTTP origin")
		f.StringVar(&o.ApplicationInternalURL, "application-internal-url", "", "Application Server internal HTTP origin")
		f.StringVar(&o.SystemURL, "system-url", "", "System Administration origin")
		f.StringVar(&o.ZoneURL, "zone-url", "", "Zone origin")
		f.StringVar(&o.SiteURL, "site-url", "", "Site origin")
		f.StringVar(&o.ApplicationURL, "application-url", "", "Application Server origin")
		f.StringVar(&o.TLSCertificate, "tls-cert", "", "HTTPS certificate file")
		f.StringVar(&o.TLSKey, "tls-key", "", "HTTPS private key file")
		f.StringVar(&o.AllowedCIDR, "allowed-cidr", "", "allowed client network; loopback always allowed")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *config == "" || o.Root == "" {
			return errors.New("init requires --config and --root")
		}
		info, err := os.Stat(*config)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0077 != 0 {
			return errors.New("initial configuration contains an administrator verifier; make it owner-readable only")
		}
		file, err := os.Open(*config)
		if err != nil {
			return err
		}
		defer file.Close()
		seed, err := framework.DecodeInitialConfig(file)
		if err != nil {
			return err
		}
		receipt, err := framework.Initialize(seed, o)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(receipt)
	}
	if args[0] == "radio" {
		return runRadio(args[1:])
	}
	if args[0] == "history-rebuild" {
		f := flag.NewFlagSet("history-rebuild", flag.ContinueOnError)
		path := f.String("database", "", "activity database of a stopped service")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *path == "" || f.NArg() != 0 {
			return errors.New("history-rebuild requires --database PATH")
		}
		return activity.RebuildHistory(*path)
	}
	if args[0] == "enroll-site" || args[0] == "enroll-application" {
		f := flag.NewFlagSet(args[0], flag.ContinueOnError)
		root := f.String("root", "", "framework directory")
		idFlag := "site-id"
		enroll := framework.EnrollSite
		if args[0] == "enroll-application" {
			idFlag = "application-id"
			enroll = framework.EnrollApplication
		}
		id := f.String(idFlag, "", "new service identity")
		address := f.String("public-url", "", "new service HTTPS origin")
		listen := f.String("listen", "", "new service web listen host:port")
		internalURL := f.String("internal-url", "", "new service internal HTTP origin")
		internalListen := f.String("internal-listen", "", "new service VPN listen host:port")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *root == "" || *id == "" || *address == "" || *internalURL == "" {
			return fmt.Errorf("%s requires --root, --%s, --public-url and --internal-url", args[0], idFlag)
		}
		path, err := enroll(*root, *id, *address, *listen, *internalURL, *internalListen)
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	}
	role := framework.Role(args[0])
	if !role.Valid() {
		return errors.New("unknown service; use system, zone, site or application")
	}
	f := flag.NewFlagSet(string(role), flag.ContinueOnError)
	path := f.String("config", "", "this service's service.json file")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *path == "" {
		return errors.New("provide --config with this service's service.json")
	}
	service, err := framework.OpenService(*path, frameworkui.Assets())
	if err != nil {
		return err
	}
	defer service.Close()
	if service.Config().Role != role {
		return errors.New("the configuration belongs to a different service")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return service.Run(ctx)
}

func runRadio(args []string) error {
	if len(args) == 0 {
		return errors.New("use radio start, stop or status --config SITE_SERVICE_JSON")
	}
	f := flag.NewFlagSet("radio "+args[0], flag.ContinueOnError)
	path := f.String("config", "", "running Site's service.json file")
	channel := f.String("channel", "", "voice channel record ID; omit for the whole Site")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || f.NArg() != 0 {
		return errors.New("radio requires --config with the Site's service.json")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	status, err := framework.ControlRadio(ctx, *path, args[0], *channel)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}
