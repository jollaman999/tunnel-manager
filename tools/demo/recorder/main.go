// Command recorder drives the tunnel-manager UI in a headless Chrome and saves
// a screenshot after every step. The frames and a list of how long each one is
// held are what run.sh turns into docs/demo.gif.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

type config struct {
	base            string
	passwordFile    string
	frames          string
	chrome          string
	account         string
	accountPassword string
	serviceIP       string
	servicePorts    []string
	localPorts      []string
	hosts           []string
	hostAddresses   map[string]string
	hostRoutes      map[string][]string
	hopHosts        map[string]bool
	serviceHost     string
	forwardHost     string
	pauseHost       string
	hostUser        string
	hostPassword    string
	forwardPort     string
	targetIP        string
	targetPort      string
	socksPort       string
	socksURL        string
	width           int
	height          int
	pace            float64
}

func main() {
	var cfg config

	flag.StringVar(&cfg.base, "base", "https://127.0.0.1:8888", "address tunnel-manager serves the UI on")
	flag.StringVar(&cfg.passwordFile, "initial-password-file", "", "the initial-password file of the demo installation")
	flag.StringVar(&cfg.frames, "frames", "frames", "directory the frames and frames.txt are written to")
	flag.StringVar(&cfg.chrome, "chrome", "/usr/bin/google-chrome", "Chrome executable")
	flag.StringVar(&cfg.account, "account", "admin", "username the setup gives the account")
	flag.StringVar(&cfg.accountPassword, "account-password", "demo-account-password", "password the setup gives the account")
	flag.StringVar(&cfg.serviceIP, "service-ip", "127.0.0.1", "address of the demo service")
	servicePorts := flag.String("service-ports", "8000,8001,8002", "comma-separated ports of the demo service")
	localPorts := flag.String("local-ports", "8080,8081,8082", "comma-separated ports the Host opens for them, one for each")
	hosts := flag.String("hosts", "", "comma-separated names of the Hosts, in the order they are added")
	hostAddresses := flag.String("host-addresses", "", "comma-separated name=address:port of every Host, the address it is registered at")
	hostRoutes := flag.String("host-routes", "", "comma-separated name=hop+hop of the Hosts reached through others, the hops in order")
	hopHosts := flag.String("hop-hosts", "", "comma-separated names of the Hosts that carry no service port")
	flag.StringVar(&cfg.serviceHost, "service-host", "", "the Host whose ports for the service ports are opened")
	flag.StringVar(&cfg.forwardHost, "forward-host", "", "the Host the local forward and the SOCKS5 proxy are made on")
	flag.StringVar(&cfg.pauseHost, "pause-host", "", "the Host on the way that is switched off for a moment")
	flag.StringVar(&cfg.hostUser, "host-user", "demo", "SSH user of the Host")
	flag.StringVar(&cfg.hostPassword, "host-password", "demo-host-password", "SSH password of the Host")
	flag.StringVar(&cfg.forwardPort, "forward-port", "18080", "port the local forward opens on this machine")
	flag.StringVar(&cfg.targetIP, "target-ip", "127.0.0.1", "address the local forward reaches from the Host")
	flag.StringVar(&cfg.targetPort, "target-port", "80", "port the local forward reaches from the Host")
	flag.StringVar(&cfg.socksPort, "socks-port", "1080", "port the SOCKS5 proxy of the Host listens on here")
	flag.StringVar(&cfg.socksURL, "socks-url", "http://127.0.0.1/", "address opened through the SOCKS5 proxy, as the Host reaches it")
	flag.IntVar(&cfg.width, "width", 1440, "viewport width")
	flag.IntVar(&cfg.height, "height", 900, "viewport height")
	flag.Float64Var(&cfg.pace, "pace", 1.15, "how many times as long every frame is held as the recording asks for")
	flag.Parse()

	if cfg.passwordFile == "" {
		log.Fatal("-initial-password-file is required")
	}

	if cfg.pace <= 0 {
		log.Fatal("-pace must be more than 0")
	}

	cfg.servicePorts = strings.Split(*servicePorts, ",")
	cfg.localPorts = strings.Split(*localPorts, ",")

	if len(cfg.servicePorts) != len(cfg.localPorts) {
		log.Fatal("-service-ports and -local-ports must name as many ports as each other")
	}

	if err := cfg.readHosts(*hosts, *hostAddresses, *hostRoutes, *hopHosts); err != nil {
		log.Fatal(err)
	}

	if err := record(cfg); err != nil {
		log.Fatal(err)
	}
}

// readHosts takes the Hosts out of the flags that name them and checks that
// every Host the other flags name is one of them.
func (cfg *config) readHosts(hosts, addresses, routes, hops string) error {
	cfg.hosts = strings.Split(hosts, ",")
	cfg.hostAddresses = map[string]string{}
	cfg.hostRoutes = map[string][]string{}
	cfg.hopHosts = map[string]bool{}

	known := map[string]bool{}

	for _, name := range cfg.hosts {
		known[name] = true
	}

	for _, pair := range strings.Split(addresses, ",") {
		name, address, ok := strings.Cut(pair, "=")
		if !ok || !known[name] {
			return fmt.Errorf("-host-addresses names %q, which is not one of -hosts", pair)
		}

		if _, _, err := net.SplitHostPort(address); err != nil {
			return fmt.Errorf("-host-addresses: %s: %w", name, err)
		}

		cfg.hostAddresses[name] = address
	}

	for _, name := range cfg.hosts {
		if cfg.hostAddresses[name] == "" {
			return fmt.Errorf("-host-addresses has no address for %s", name)
		}
	}

	if routes != "" {
		for _, pair := range strings.Split(routes, ",") {
			name, route, ok := strings.Cut(pair, "=")
			if !ok || !known[name] {
				return fmt.Errorf("-host-routes names %q, which is not one of -hosts", pair)
			}

			for _, hop := range strings.Split(route, "+") {
				if !known[hop] {
					return fmt.Errorf("-host-routes: the route of %s passes %q, which is not one of -hosts", name, hop)
				}

				cfg.hostRoutes[name] = append(cfg.hostRoutes[name], hop)
			}
		}
	}

	if hops != "" {
		for _, name := range strings.Split(hops, ",") {
			if !known[name] {
				return fmt.Errorf("-hop-hosts names %q, which is not one of -hosts", name)
			}

			cfg.hopHosts[name] = true
		}
	}

	for flagName, name := range map[string]string{
		"-service-host": cfg.serviceHost, "-forward-host": cfg.forwardHost, "-pause-host": cfg.pauseHost,
	} {
		if !known[name] {
			return fmt.Errorf("%s names %q, which is not one of -hosts", flagName, name)
		}
	}

	return nil
}

// tunnels is how many service port tunnels there are once every Host carries
// what it was added with.
func (cfg config) tunnels() int {
	return (len(cfg.hosts) - len(cfg.hopHosts)) * len(cfg.servicePorts)
}

// hostOf is the address a Host is registered at, apart from its port.
func (cfg config) hostOf(name string) (string, string) {
	host, port, _ := net.SplitHostPort(cfg.hostAddresses[name])

	return host, port
}

func record(cfg config) error {
	initial, err := os.ReadFile(cfg.passwordFile)
	if err != nil {
		return fmt.Errorf("reading the initial password: %w", err)
	}

	if err := os.MkdirAll(cfg.frames, 0o755); err != nil {
		return err
	}

	options := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options,
		chromedp.ExecPath(cfg.chrome),
		chromedp.WindowSize(cfg.width, cfg.height),
		chromedp.Flag("ignore-certificate-errors", true),
		chromedp.Flag("lang", "en-US"),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), options...)
	defer cancelAlloc()

	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	defer cancelCtx()

	ctx, cancelTimeout := context.WithTimeout(ctx, 15*time.Minute)
	defer cancelTimeout()

	r := &recorder{ctx: ctx, dir: cfg.frames, options: options, width: cfg.width, height: cfg.height, pace: cfg.pace}

	if err := chromedp.Run(ctx, chromedp.EmulateViewport(int64(cfg.width), int64(cfg.height))); err != nil {
		return fmt.Errorf("starting Chrome: %w", err)
	}

	scenes := []struct {
		name string
		run  func(*recorder, config, string) error
	}{
		{"sign in", sceneSignIn},
		{"service port", sceneServicePort},
		{"host", sceneHost},
		{"hosts behind it", sceneJumpHosts},
		{"status", sceneStatus},
		{"jump routes", sceneJumpRoutes},
		{"service", sceneService},
		{"local forward", sceneLocalForward},
		{"status again", sceneStatusBoth},
		{"hop switched off", sceneHopOff},
		{"socks5", sceneSOCKS},
	}

	for _, scene := range scenes {
		if err := scene.run(r, cfg, strings.TrimSpace(string(initial))); err != nil {
			_ = r.shot(time.Second)
			_ = r.writeList()

			return fmt.Errorf("scene %q: %w", scene.name, err)
		}
	}

	r.hold(3 * time.Second)

	return r.writeList()
}

func sceneSignIn(r *recorder, cfg config, initial string) error {
	if err := r.navigate(cfg.base + "/ui/login"); err != nil {
		return err
	}

	if err := r.run(
		chromedp.Evaluate(`localStorage.setItem("tm_lang", "en")`, nil),
		chromedp.Reload(),
		chromedp.WaitVisible(`#login-password`, chromedp.ByQuery),
	); err != nil {
		return err
	}

	if err := r.caption("1. Sign in with the initial password and set up the account"); err != nil {
		return err
	}

	r.pause(1500 * time.Millisecond)

	if err := r.typeInto(`#login-password`, initial, 8); err != nil {
		return err
	}

	if err := r.click(`[data-action="login-submit"]`, `#setup-username`); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.typeInto(`#setup-username`, cfg.account, 2); err != nil {
		return err
	}

	if err := r.typeInto(`#setup-password`, cfg.accountPassword, 4); err != nil {
		return err
	}

	if err := r.typeInto(`#setup-password_confirmation`, cfg.accountPassword, 4); err != nil {
		return err
	}

	if err := r.click(`[data-action="setup-submit"]`, `nav a.current[data-screen="status"]`); err != nil {
		return err
	}

	r.pause(1500 * time.Millisecond)

	return nil
}

// sceneServicePort adds the first service port at the pace of the rest of the
// recording, and the others the same way but quickly: once the first has been
// seen, the others only need to be seen going in.
func sceneServicePort(r *recorder, cfg config, _ string) error {
	if err := r.click(`nav a[data-screen="service-ports"]`, `#service-port-create-service_address`); err != nil {
		return err
	}

	if err := r.caption("2. Add the service to publish, and the port the Hosts open for it"); err != nil {
		return err
	}

	r.pause(time.Second)

	for i, servicePort := range cfg.servicePorts {
		if i == 1 {
			if err := r.caption("2. Add the other ports of the service the same way"); err != nil {
				return err
			}

			r.fast = true
		}

		fields := []struct{ sel, text string }{
			{`#service-port-create-service_address`, cfg.serviceIP},
			{`#service-port-create-service_port`, servicePort},
			{`#service-port-create-local_port`, cfg.localPorts[i]},
			{`#service-port-create-description`, fmt.Sprintf("Demo web service %d", i+1)},
		}

		for _, field := range fields {
			if err := r.typeInto(field.sel, field.text, 3); err != nil {
				return err
			}
		}

		if err := r.click(`[data-action="service-port-create-submit"]`, ``); err != nil {
			return err
		}

		added := fmt.Sprintf(`document.querySelectorAll('[data-action^="service-port-edit-"]').length >= %d`, i+1)

		if err := r.waitTrue(added, fmt.Sprintf("service port %d in the table", i+1), 20*time.Second); err != nil {
			return err
		}

		if err := r.scrollTo(`table`); err != nil {
			return err
		}

		// The table with all of them in it is held as long as the first one
		// was, so that what the quick part added can be read.
		if i == len(cfg.servicePorts)-1 {
			r.fast = false
		}

		r.pause(1500 * time.Millisecond)
	}

	return nil
}

func sceneHost(r *recorder, cfg config, _ string) error {
	if err := r.click(`nav a[data-screen="hosts"]`, `#host-create-address`); err != nil {
		return err
	}

	if err := r.caption("3. Add the Host this machine reaches directly, the bastion"); err != nil {
		return err
	}

	r.pause(time.Second)

	return r.addHost(cfg, cfg.hosts[0])
}

// sceneJumpHosts adds the Hosts reached through others: the first at the pace
// of the rest of the recording, with the panel its route is set in, and the
// others the same way but quickly.
func sceneJumpHosts(r *recorder, cfg config, _ string) error {
	for i, name := range cfg.hosts[1:] {
		caption := fmt.Sprintf("4. Add %s behind it: its jump route passes through the bastion", name)

		if i > 0 {
			caption = "4. Add the other Hosts the same way, one or two hops deep"
			r.fast = true
		}

		if err := r.caption(caption); err != nil {
			return err
		}

		r.pause(time.Second)

		if err := r.addHost(cfg, name); err != nil {
			return err
		}
	}

	r.fast = false

	r.pause(1500 * time.Millisecond)

	return nil
}

// addHost fills in the add form for the Host named, its jump route included,
// and adds it. A Host that only passes the others on is added without the
// service ports.
func (r *recorder) addHost(cfg config, name string) error {
	address, port := cfg.hostOf(name)

	if err := r.typeInto(`#host-create-address`, address, 3); err != nil {
		return err
	}

	if port != "22" {
		if err := r.run(chromedp.SetValue(`#host-create-port`, "", chromedp.ByQuery)); err != nil {
			return err
		}

		if err := r.typeInto(`#host-create-port`, port, 2); err != nil {
			return err
		}
	}

	if err := r.typeInto(`#host-create-user`, cfg.hostUser, 2); err != nil {
		return err
	}

	if route := cfg.hostRoutes[name]; len(route) > 0 {
		if err := r.setRoute(route); err != nil {
			return err
		}
	}

	fields := []struct {
		sel, text string
		chunk     int
	}{
		{`#host-create-password`, cfg.hostPassword, 4},
		{`#host-create-description`, name, 3},
	}

	for _, field := range fields {
		if err := r.typeInto(field.sel, field.text, field.chunk); err != nil {
			return err
		}
	}

	if cfg.hopHosts[name] {
		if err := r.click(`#host-create-assign_all_service_ports`, ``); err != nil {
			return err
		}
	}

	if err := r.click(`[data-action="host-create-submit"]`, ``); err != nil {
		return err
	}

	if err := r.waitTrue(fmt.Sprintf("%s !== \"\"", hostIDExpr(name)), name+" in the table", 20*time.Second); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	r.pause(1500 * time.Millisecond)

	return nil
}

// setRoute opens the jump route panel of the add form, puts the Hosts named on
// the route in order and saves it.
func (r *recorder) setRoute(route []string) error {
	if err := r.click(`form[data-form="host-create"] [data-action="jump-route-open"]`,
		`[data-modal-panel="jump-route"]`); err != nil {
		return err
	}

	r.pause(time.Second)

	for _, hop := range route {
		id, err := r.hostID(hop)
		if err != nil {
			return err
		}

		if err := r.click(fmt.Sprintf(`[data-modal-panel="jump-route"] [data-action="jump-pick-%s"]`, id), ``); err != nil {
			return err
		}

		if err := r.click(`[data-modal-panel="jump-route"] [data-action="jump-route-add"]`, ``); err != nil {
			return err
		}

		r.pause(time.Second)
	}

	r.pause(1500 * time.Millisecond)

	if err := r.click(`[data-modal-panel="jump-route"] [data-action="jump-route-save"]`, ``); err != nil {
		return err
	}

	if err := r.waitGone(`[data-modal-panel="jump-route"]`, 10*time.Second); err != nil {
		return err
	}

	if err := r.waitTrue(`document.querySelector('form[data-form="host-create"] .jump-steps') !== null`,
		"the route under the add form", 10*time.Second); err != nil {
		return err
	}

	if err := r.point(`form[data-form="host-create"] .jump-field`); err != nil {
		return err
	}

	r.pause(time.Second)

	return nil
}

// hostIDExpr is a script that answers with the id of the Host whose
// description is name in the table of the Hosts screen, or "" while there is no
// such row.
func hostIDExpr(name string) string {
	return fmt.Sprintf(`(function (name) {
  for (const edit of document.querySelectorAll('[data-action^="host-edit-"]')) {
    const row = edit.closest("tr");
    if (row === null) { continue; }
    for (const cell of row.querySelectorAll("td")) {
      if (cell.textContent.trim() === name) { return edit.dataset.action.slice("host-edit-".length); }
    }
  }
  return "";
})(%q)`, name)
}

// hostID is the id of the Host named, read off the table of the Hosts screen.
func (r *recorder) hostID(name string) (string, error) {
	var id string

	if err := r.run(chromedp.Evaluate(hostIDExpr(name), &id)); err != nil {
		return "", err
	}

	if id == "" {
		return "", fmt.Errorf("%s is not in the table of the Hosts screen", name)
	}

	return id, nil
}

// openHosts loads the Hosts screen and waits for the row of every Host.
func (r *recorder) openHosts(cfg config) error {
	if err := r.navigate(cfg.base + "/ui/hosts"); err != nil {
		return err
	}

	rows := fmt.Sprintf(`document.querySelectorAll('[data-action^="host-edit-"]').length >= %d`, len(cfg.hosts))

	return r.waitTrue(rows, "every Host in the table", 20*time.Second)
}

// connectedExpr is a script that is true once the status screen counts at least
// n forwards connected.
func connectedExpr(n int) string {
	return fmt.Sprintf(`(function () {
  const count = document.querySelector('[data-count="connected"] span');
  return count !== null && Number(count.textContent) >= %d;
})()`, n)
}

// sceneStatus approves the host keys as they come in. A Host behind another is
// not reached until the Host before it is trusted, so the keys come in a hop at
// a time: the first is approved from its row at the pace of the rest of the
// recording, and the ones after it are ticked together, the last of them
// quickly.
func sceneStatus(r *recorder, cfg config, _ string) error {
	if err := r.click(`nav a[data-screen="status"]`, `[data-count="connected"]`); err != nil {
		return err
	}

	if err := r.caption("5. Approve the host keys, a hop at a time, and the tunnels come up"); err != nil {
		return err
	}

	all := connectedExpr(cfg.tunnels())
	waiting := `document.querySelector('[data-action="host-keys"]') !== null`

	for round := 0; ; round++ {
		if round == len(cfg.hosts) {
			return fmt.Errorf("the host keys were still waiting after %d rounds", round)
		}

		if err := r.waitTrueWhileShooting("("+waiting+") || "+all, "a host key or every tunnel", 90*time.Second); err != nil {
			return err
		}

		var done bool

		if err := r.run(chromedp.Evaluate(all, &done)); err != nil {
			return err
		}

		if done {
			break
		}

		if round > 1 {
			r.fast = true
		}

		r.pause(time.Second)

		if err := r.click(`[data-action="host-keys"]`, `[data-modal-panel="host-keys"] .host-key-row .buttons button`); err != nil {
			return err
		}

		r.pause(time.Second)

		if round == 0 {
			if err := r.approveFromRow(); err != nil {
				return err
			}
		} else if err := r.approveTicked(); err != nil {
			return err
		}

		r.pause(time.Second)

		// The panel goes by itself once a press at its bottom has approved
		// everything ticked.
		var open bool

		if err := r.run(chromedp.Evaluate(`document.querySelector('[data-modal-panel="host-keys"]') !== null`, &open)); err != nil {
			return err
		}

		if open {
			if err := r.click(`[data-action="host-keys-close"]`, ``); err != nil {
				return err
			}
		}
	}

	r.fast = false

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.point(`.badge[data-status="connected"]`); err != nil {
		return err
	}

	return r.shot(2500 * time.Millisecond)
}

// approveFromRow approves the first Host of the host key list from its own row.
func (r *recorder) approveFromRow() error {
	if err := r.click(`[data-modal-panel="host-keys"] .host-key-row .buttons button`, `[data-action="host-key-approve"]`); err != nil {
		return err
	}

	r.pause(1500 * time.Millisecond)

	if err := r.click(`[data-action="host-key-approve"]`, ``); err != nil {
		return err
	}

	return r.waitGone(`[data-modal-panel="host-key"]`, 10*time.Second)
}

// approveTicked ticks every Host of the host key list and approves them in one
// press.
func (r *recorder) approveTicked() error {
	if err := r.click(`[data-modal-panel="host-keys"] [data-field="host-keys-all"]`, ``); err != nil {
		return err
	}

	if err := r.click(`[data-action="host-keys-approve"]`, `[data-action="host-keys-confirm-approve"]`); err != nil {
		return err
	}

	r.pause(1500 * time.Millisecond)

	if err := r.click(`[data-action="host-keys-confirm-approve"]`, ``); err != nil {
		return err
	}

	return r.waitGone(`[data-modal-panel="host-keys-confirm"]`, 10*time.Second)
}

// sceneJumpRoutes shows the route of each Host in the list: none, one hop and
// two hops.
func sceneJumpRoutes(r *recorder, cfg config, _ string) error {
	if err := r.openHosts(cfg); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.caption("6. The list shows the jump route of every Host: none, one hop or two"); err != nil {
		return err
	}

	r.pause(time.Second)

	shown := map[int]bool{}

	for _, name := range cfg.hosts {
		hops := len(cfg.hostRoutes[name])
		if shown[hops] {
			continue
		}

		shown[hops] = true

		id, err := r.hostID(name)
		if err != nil {
			return err
		}

		if err := r.point(fmt.Sprintf(`[data-action="host-jump-%s"] .jump-pill-icon`, id)); err != nil {
			return err
		}

		if err := r.shot(1500 * time.Millisecond); err != nil {
			return err
		}
	}

	if err := r.overlay("window.__demo.hide()", nil); err != nil {
		return err
	}

	return r.shot(1000 * time.Millisecond)
}

// sceneService opens the port the Host opened for each service port, the first
// at the pace of the rest of the recording and the others quickly, the way
// sceneServicePort adds them. Each page names the port of the service it was
// served on.
func sceneService(r *recorder, cfg config, _ string) error {
	host, _ := cfg.hostOf(cfg.serviceHost)

	for i, localPort := range cfg.localPorts {
		address := fmt.Sprintf("http://%s/", net.JoinHostPort(host, localPort))

		if err := r.navigate(address); err != nil {
			return err
		}

		var heading, port string

		if err := r.run(
			chromedp.WaitVisible(`h1`, chromedp.ByQuery),
			chromedp.Text(`h1`, &heading, chromedp.ByQuery),
			chromedp.Text(`main p code`, &port, chromedp.ByQuery),
		); err != nil {
			return err
		}

		if !strings.Contains(heading, "Hello from the service behind the tunnel") {
			return fmt.Errorf("the page at %s says %q and not the demo service", address, heading)
		}

		if port != cfg.servicePorts[i] {
			return fmt.Errorf("the page at %s names port %q and not %s", address, port, cfg.servicePorts[i])
		}

		caption := fmt.Sprintf("7. A client opens the port on %s and reaches the service", cfg.serviceHost)

		if i > 0 {
			caption = fmt.Sprintf("7. The other ports on %s reach the other ports of the service", cfg.serviceHost)
			r.fast = true
		}

		if err := r.caption(caption); err != nil {
			return err
		}

		if err := r.shot(3500 * time.Millisecond); err != nil {
			return err
		}
	}

	r.fast = false

	return nil
}

func sceneLocalForward(r *recorder, cfg config, _ string) error {
	if err := r.openHosts(cfg); err != nil {
		return err
	}

	id, err := r.hostID(cfg.forwardHost)
	if err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.caption(fmt.Sprintf("8. Open a port here that reaches a service only %s, two hops away, can reach",
		cfg.forwardHost)); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.rowAction("host-local-forwards-"+id, `[data-action="local-forward-add"]`); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.click(`[data-action="local-forward-add"]`, `#local-forward-create-local_port`); err != nil {
		return err
	}

	r.pause(time.Second)

	fields := []struct{ sel, text string }{
		{`#local-forward-create-local_port`, cfg.forwardPort},
		{`#local-forward-create-target_address`, cfg.targetIP},
		{`#local-forward-create-target_port`, cfg.targetPort},
		{`#local-forward-create-description`, "Web inside " + cfg.forwardHost},
	}

	for _, field := range fields {
		if err := r.typeInto(field.sel, field.text, 3); err != nil {
			return err
		}
	}

	if err := r.click(`[data-action="local-forward-create-submit"]`, ``); err != nil {
		return err
	}

	if err := r.waitGone(`[data-modal-panel="local-forward-add"]`, 10*time.Second); err != nil {
		return err
	}

	// The recording moves on without waiting for the new row to say it is
	// connected; the status screen shows that later. Until the forward answers,
	// the page below would not load, so that is waited for off camera.
	if err := r.click(`[data-action="local-forwards-close"]`, ``); err != nil {
		return err
	}

	address := fmt.Sprintf("http://127.0.0.1:%s/", cfg.forwardPort)

	if err := waitAnswers(address, "Hello from inside the Host", 60*time.Second); err != nil {
		return err
	}

	if err := r.navigate(address); err != nil {
		return err
	}

	var heading string

	if err := r.run(
		chromedp.WaitVisible(`h1`, chromedp.ByQuery),
		chromedp.Text(`h1`, &heading, chromedp.ByQuery),
	); err != nil {
		return err
	}

	if !strings.Contains(heading, "Hello from inside the Host") {
		return fmt.Errorf("the page at %s says %q and not the web server inside the Host", address, heading)
	}

	if err := r.caption(fmt.Sprintf("9. This machine opens the port and reaches the web server inside %s",
		cfg.forwardHost)); err != nil {
		return err
	}

	return r.shot(3500 * time.Millisecond)
}

// sceneStatusBoth goes back to the status screen, where the local forward that
// was just made stands in the same table as the tunnel.
func sceneStatusBoth(r *recorder, cfg config, _ string) error {
	if err := r.navigate(cfg.base + "/ui/status"); err != nil {
		return err
	}

	// The last of the four count boxes, so that seeing it means the row of
	// them has been drawn rather than only begun.
	if err := r.run(chromedp.WaitVisible(`[data-count="errors"]`, chromedp.ByQuery)); err != nil {
		return err
	}

	if err := r.caption("10. The status screen holds the tunnels and the local forward in one table"); err != nil {
		return err
	}

	// The count takes both sorts of row, so what is waited for is one more
	// than there are service port tunnels.
	both := connectedExpr(cfg.tunnels() + 1)

	if err := r.waitTrueWhileShooting(both, "both rows to be connected", 60*time.Second); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	return r.shot(3500 * time.Millisecond)
}

// sceneHopOff switches off the Host on the way for a moment: the Hosts behind it
// say in the list that their route stops there, the status screen says at
// which hop and what to do, and switching it on again brings the route back.
func sceneHopOff(r *recorder, cfg config, _ string) error {
	if err := r.openHosts(cfg); err != nil {
		return err
	}

	hop, err := r.hostID(cfg.pauseHost)
	if err != nil {
		return err
	}

	behind, err := r.hostID(cfg.forwardHost)
	if err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.caption(fmt.Sprintf("11. Switch %s off and the Hosts behind it show their route stops there",
		cfg.pauseHost)); err != nil {
		return err
	}

	r.pause(time.Second)

	toggle := "host-toggle-" + hop
	pill := fmt.Sprintf(`[data-action="host-jump-%s"]`, behind)

	if err := r.rowAction(toggle, ``); err != nil {
		return err
	}

	if err := r.waitTrue(fmt.Sprintf(`(function () {
  const pill = document.querySelector(%q);
  return pill !== null && pill.dataset.jump === "warn";
})()`, pill), "the route of "+cfg.forwardHost+" to stop", 20*time.Second); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.point(pill + ` .jump-pill-icon`); err != nil {
		return err
	}

	if err := r.shot(2500 * time.Millisecond); err != nil {
		return err
	}

	if err := r.navigate(cfg.base + "/ui/status"); err != nil {
		return err
	}

	if err := r.run(chromedp.WaitVisible(`[data-count="errors"]`, chromedp.ByQuery)); err != nil {
		return err
	}

	if err := r.caption("11. The status screen says which hop is off and what to do about it"); err != nil {
		return err
	}

	note := `.jump-note[data-jump-reason="disabled"]`

	if err := r.waitWhileShooting(note, 60*time.Second); err != nil {
		return err
	}

	if err := r.point(note); err != nil {
		return err
	}

	if err := r.shot(3500 * time.Millisecond); err != nil {
		return err
	}

	if err := r.openHosts(cfg); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.caption(fmt.Sprintf("11. Switch %s on again and the route is back", cfg.pauseHost)); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.rowAction(toggle, ``); err != nil {
		return err
	}

	if err := r.waitTrue(fmt.Sprintf(`(function () {
  const pill = document.querySelector(%q);
  return pill !== null && pill.dataset.jump === "via";
})()`, pill), "the route of "+cfg.forwardHost+" to come back", 20*time.Second); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.point(pill + ` .jump-pill-icon`); err != nil {
		return err
	}

	return r.shot(2000 * time.Millisecond)
}

func sceneSOCKS(r *recorder, cfg config, _ string) error {
	if err := r.openHosts(cfg); err != nil {
		return err
	}

	id, err := r.hostID(cfg.forwardHost)
	if err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.caption(fmt.Sprintf("12. Turn on a SOCKS5 proxy on %s and browse the network behind it",
		cfg.forwardHost)); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.rowAction("host-edit-"+id, `#host-edit-socks_enabled`); err != nil {
		return err
	}

	if err := r.click(`#host-edit-socks_enabled`, `#host-edit-socks_port`); err != nil {
		return err
	}

	if err := r.point(`#host-edit-socks_port`); err != nil {
		return err
	}

	if err := r.shot(1500 * time.Millisecond); err != nil {
		return err
	}

	if err := r.click(`[data-action="host-edit-submit"]`, `[data-socks]`); err != nil {
		return err
	}

	connected := `[data-socks] .badge[data-status="connected"]`

	for tries := 0; ; tries++ {
		if err := r.scrollTo(`table`); err != nil {
			return err
		}

		var found bool

		if err := r.run(chromedp.Evaluate(fmt.Sprintf("document.querySelector(%q) !== null", connected), &found)); err != nil {
			return err
		}

		if found {
			break
		}

		if tries == 30 {
			return fmt.Errorf("the SOCKS5 proxy did not connect")
		}

		if err := r.shot(time.Second); err != nil {
			return err
		}

		time.Sleep(time.Second)

		if err := r.run(chromedp.Click(`nav a[data-screen="hosts"]`, chromedp.ByQuery),
			chromedp.WaitVisible(`[data-socks]`, chromedp.ByQuery)); err != nil {
			return err
		}
	}

	if err := r.point(connected); err != nil {
		return err
	}

	if err := r.shot(2500 * time.Millisecond); err != nil {
		return err
	}

	return r.throughProxy(cfg)
}

// throughProxy opens cfg.socksURL in a second Chrome that sends that address,
// and nothing else, through the SOCKS5 proxy, and takes its frames into the
// same recording.
func (r *recorder) throughProxy(cfg config) error {
	pac := fmt.Sprintf(`function FindProxyForURL(url, host) {
  return url.indexOf(%q) === 0 ? "SOCKS5 127.0.0.1:%s" : "DIRECT";
}`, cfg.socksURL, cfg.socksPort)

	options := append([]chromedp.ExecAllocatorOption{}, r.options...)
	options = append(options,
		chromedp.Flag("proxy-pac-url", "data:application/x-ns-proxy-autoconfig;base64,"+
			base64.StdEncoding.EncodeToString([]byte(pac))),
	)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), options...)
	defer cancelAlloc()

	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	defer cancelCtx()

	ctx, cancelTimeout := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelTimeout()

	outer := r.ctx
	r.ctx = ctx

	defer func() {
		r.ctx = outer
	}()

	if err := r.run(chromedp.EmulateViewport(int64(r.width), int64(r.height))); err != nil {
		return fmt.Errorf("starting the Chrome behind the proxy: %w", err)
	}

	if err := r.navigate(cfg.socksURL); err != nil {
		return err
	}

	var heading string

	if err := r.run(
		chromedp.WaitVisible(`h1`, chromedp.ByQuery),
		chromedp.Text(`h1`, &heading, chromedp.ByQuery),
	); err != nil {
		return err
	}

	if !strings.Contains(heading, "Hello from inside the Host") {
		return fmt.Errorf("the page at %s through the proxy says %q and not the web server inside the Host", cfg.socksURL, heading)
	}

	if err := r.caption(fmt.Sprintf("13. A browser set to the SOCKS5 proxy opens %s as %s sees it",
		cfg.socksURL, cfg.forwardHost)); err != nil {
		return err
	}

	return r.shot(3500 * time.Millisecond)
}

type frame struct {
	file string
	hold time.Duration
}

type recorder struct {
	ctx     context.Context
	dir     string
	frames  []frame
	last    []byte
	options []chromedp.ExecAllocatorOption
	width   int
	height  int

	// fast cuts every hold to a fifth and types a field in one go, for steps
	// that repeat one the recording has already shown at its own pace.
	fast bool

	// pace is how many times as long every hold is as the step asks for, so
	// that the whole recording plays slower or quicker at once.
	pace float64
}

func (r *recorder) run(actions ...chromedp.Action) error {
	return chromedp.Run(r.ctx, actions...)
}

// navigate loads url, and loads it again when Chrome gave up on it because the
// network of the machine changed under it. Starting a container adds an
// interface and an address to this machine a second or so later, and a load
// that is in flight at that moment fails with ERR_NETWORK_CHANGED.
func (r *recorder) navigate(url string) error {
	var err error

	for range 3 {
		err = r.run(chromedp.Navigate(url))
		if err == nil || !strings.Contains(err.Error(), "ERR_NETWORK_CHANGED") {
			return err
		}

		time.Sleep(2 * time.Second)
	}

	return err
}

// shot saves what the viewport shows and holds it for hold. A frame that is the
// same as the one before it only lengthens that one.
func (r *recorder) shot(hold time.Duration) error {
	hold = r.held(hold)

	var png []byte

	if err := r.run(chromedp.CaptureScreenshot(&png)); err != nil {
		return err
	}

	if len(r.frames) > 0 && bytes.Equal(png, r.last) {
		r.frames[len(r.frames)-1].hold += hold

		return nil
	}

	name := fmt.Sprintf("%05d.png", len(r.frames)+1)

	if err := os.WriteFile(filepath.Join(r.dir, name), png, 0o644); err != nil {
		return err
	}

	r.frames = append(r.frames, frame{file: name, hold: hold})
	r.last = png

	return nil
}

func (r *recorder) hold(d time.Duration) {
	if len(r.frames) > 0 {
		r.frames[len(r.frames)-1].hold += r.held(d)
	}
}

func (r *recorder) held(d time.Duration) time.Duration {
	d = time.Duration(float64(d) * r.pace)

	if r.fast {
		return d / 5
	}

	return d
}

func (r *recorder) pause(d time.Duration) {
	if err := r.shot(d); err != nil {
		r.hold(d)
	}
}

const overlay = `(function () {
  if (window.__demo) { return; }
  window.__demo = {
    caption: function (text) {
      var el = document.getElementById("demo-caption");
      if (!el) {
        el = document.createElement("div");
        el.id = "demo-caption";
        el.style.cssText = "position:fixed;left:50%;bottom:24px;transform:translateX(-50%);" +
          "z-index:2147483647;pointer-events:none;white-space:nowrap;text-align:center;" +
          "background:rgba(24,98,196,0.94);color:#fff;font:600 20px system-ui,sans-serif;" +
          "padding:10px 24px;border-radius:12px;box-shadow:0 6px 24px rgba(0,0,0,0.4)";
        document.body.appendChild(el);
        document.body.style.paddingBottom = "110px";
      }
      el.textContent = text;
    },
    point: function (selector) {
      var target = document.querySelector(selector);
      if (!target) { return false; }
      var box = target.getBoundingClientRect();
      if (box.top < 0 || box.bottom > window.innerHeight) {
        target.scrollIntoView({ block: "center" });
        box = target.getBoundingClientRect();
      }
      var dot = document.getElementById("demo-cursor");
      if (!dot) {
        dot = document.createElement("div");
        dot.id = "demo-cursor";
        dot.style.cssText = "position:fixed;width:26px;height:26px;margin:-13px 0 0 -13px;" +
          "border-radius:50%;z-index:2147483647;pointer-events:none;" +
          "background:rgba(255,200,40,0.55);border:2px solid #ffc828";
        document.body.appendChild(dot);
      }
      dot.style.left = (box.left + box.width / 2) + "px";
      dot.style.top = (box.top + box.height / 2) + "px";
      dot.style.display = "block";
      return true;
    },
    hide: function () {
      var dot = document.getElementById("demo-cursor");
      if (dot) { dot.style.display = "none"; }
    },
    scrollTo: function (selector) {
      var target = document.querySelector(selector);
      if (target) { window.scrollBy(0, target.getBoundingClientRect().bottom - (window.innerHeight - 100)); }
    }
  };
})();`

func (r *recorder) overlay(call string, result any) error {
	return r.run(chromedp.Evaluate(overlay+call, result))
}

func (r *recorder) caption(text string) error {
	return r.overlay(fmt.Sprintf("window.__demo.caption(%q)", text), nil)
}

func (r *recorder) point(sel string) error {
	var found bool

	if err := r.overlay(fmt.Sprintf("window.__demo.point(%q)", sel), &found); err != nil {
		return err
	}

	if !found {
		return fmt.Errorf("nothing on the page matches %s", sel)
	}

	return nil
}

func (r *recorder) scrollTo(sel string) error {
	return r.overlay(fmt.Sprintf("window.__demo.scrollTo(%q)", sel), nil)
}

// typeInto types text a few characters at a time, with a frame after each, so
// that the recording shows it being typed.
func (r *recorder) typeInto(sel, text string, chunk int) error {
	if err := r.waitVisible(sel, 20*time.Second); err != nil {
		return err
	}

	if err := r.point(sel); err != nil {
		return err
	}

	if err := r.run(chromedp.Focus(sel, chromedp.ByQuery)); err != nil {
		return err
	}

	runes := []rune(text)

	if r.fast {
		chunk = len(runes)
	}

	for start := 0; start < len(runes); start += chunk {
		end := min(start+chunk, len(runes))

		if err := r.run(chromedp.SendKeys(sel, string(runes[start:end]), chromedp.ByQuery)); err != nil {
			return err
		}

		if err := r.shot(90 * time.Millisecond); err != nil {
			return err
		}
	}

	r.hold(400 * time.Millisecond)

	return nil
}

// click puts the pointer on sel, clicks it and waits for then to be on the
// page. An empty then waits for nothing.
func (r *recorder) click(sel, then string) error {
	if err := r.waitVisible(sel, 20*time.Second); err != nil {
		return err
	}

	if err := r.point(sel); err != nil {
		return err
	}

	if err := r.shot(600 * time.Millisecond); err != nil {
		return err
	}

	if err := r.run(chromedp.Click(sel, chromedp.ByQuery)); err != nil {
		return err
	}

	if then != "" {
		ctx, cancel := context.WithTimeout(r.ctx, 20*time.Second)
		defer cancel()

		if err := chromedp.Run(ctx, chromedp.WaitVisible(then, chromedp.ByQuery)); err != nil {
			return fmt.Errorf("waiting for %s after clicking %s: %w", then, sel, err)
		}
	}

	time.Sleep(300 * time.Millisecond)

	if err := r.overlay("window.__demo.hide()", nil); err != nil {
		return err
	}

	return r.shot(700 * time.Millisecond)
}

// rowAction presses the button of a table row whose data-action is action, and
// waits for then the way click does. Where the table is too wide for its box
// the buttons of the rows are folded into a menu, and the press goes through
// that menu.
func (r *recorder) rowAction(action, then string) error {
	sel := fmt.Sprintf(`[data-action=%q]`, action)

	var folded bool

	if err := r.run(chromedp.Evaluate(fmt.Sprintf(`(function (sel) {
  const button = document.querySelector(sel);
  if (button === null || button.getClientRects().length > 0) { return false; }
  const old = document.getElementById("demo-menu");
  if (old !== null) { old.removeAttribute("id"); }
  const menu = button.parentElement.querySelector(":scope > .bar-menu");
  if (menu === null) { return false; }
  menu.id = "demo-menu";
  return true;
})(%q)`, sel), &folded)); err != nil {
		return err
	}

	if !folded {
		return r.click(sel, then)
	}

	item := `.action-menu-list ` + sel

	if err := r.click(`#demo-menu`, item); err != nil {
		return err
	}

	return r.click(item, then)
}

// waitVisible waits for sel to be visible on the page for at most limit.
func (r *recorder) waitVisible(sel string, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(r.ctx, limit)
	defer cancel()

	if err := chromedp.Run(ctx, chromedp.WaitVisible(sel, chromedp.ByQuery)); err != nil {
		return fmt.Errorf("waiting for %s: %w", sel, err)
	}

	return nil
}

// waitWhileShooting takes a frame a second until sel is on the page.
func (r *recorder) waitWhileShooting(sel string, limit time.Duration) error {
	return r.waitTrueWhileShooting(fmt.Sprintf("document.querySelector(%q) !== null", sel), sel, limit)
}

// waitTrueWhileShooting takes a frame a second until expr is true on the page.
// what names what was waited for when it never is.
func (r *recorder) waitTrueWhileShooting(expr, what string, limit time.Duration) error {
	deadline := time.Now().Add(limit)

	for time.Now().Before(deadline) {
		var found bool

		if err := r.run(chromedp.Evaluate(expr, &found)); err != nil {
			return err
		}

		if found {
			return r.shot(500 * time.Millisecond)
		}

		if err := r.shot(time.Second); err != nil {
			return err
		}

		time.Sleep(time.Second)
	}

	return fmt.Errorf("%s did not show up within %s", what, limit)
}

// waitTrue waits until expr is true on the page without taking frames.
func (r *recorder) waitTrue(expr, what string, limit time.Duration) error {
	deadline := time.Now().Add(limit)

	for time.Now().Before(deadline) {
		var found bool

		if err := r.run(chromedp.Evaluate(expr, &found)); err != nil {
			return err
		}

		if found {
			return nil
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("waited %s for %s", limit, what)
}

// waitAnswers waits, without taking frames, until address answers with a page
// that holds want.
func waitAnswers(address, want string, limit time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(limit)

	for time.Now().Before(deadline) {
		resp, err := client.Get(address)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if strings.Contains(string(body), want) {
				return nil
			}
		}

		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("%s did not answer with %q within %s", address, want, limit)
}

func (r *recorder) waitGone(sel string, limit time.Duration) error {
	deadline := time.Now().Add(limit)

	for time.Now().Before(deadline) {
		var found bool

		if err := r.run(chromedp.Evaluate(fmt.Sprintf("document.querySelector(%q) !== null", sel), &found)); err != nil {
			return err
		}

		if !found {
			return r.shot(700 * time.Millisecond)
		}

		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("%s was still on the page after %s", sel, limit)
}

// writeList writes frames.txt in the form the ffmpeg concat demuxer reads. The
// last frame is named twice because the demuxer drops the hold of the last
// entry.
func (r *recorder) writeList() error {
	if len(r.frames) == 0 {
		return errors.New("no frames were taken")
	}

	var list strings.Builder

	for _, f := range r.frames {
		fmt.Fprintf(&list, "file '%s'\nduration %.3f\n", f.file, f.hold.Seconds())
	}

	fmt.Fprintf(&list, "file '%s'\n", r.frames[len(r.frames)-1].file)

	return os.WriteFile(filepath.Join(r.dir, "frames.txt"), []byte(list.String()), 0o644)
}
