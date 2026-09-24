// Command recorder drives the tunnel-manager UI in a headless Chrome and saves
// a screenshot after every step. The frames and a list of how long each one is
// held are what run.sh turns into docs/demo.gif.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
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
	servicePort     string
	localPort       string
	hostIP          string
	hostPort        string
	hostUser        string
	hostPassword    string
	serviceURL      string
	forwardPort     string
	targetIP        string
	targetPort      string
	socksPort       string
	socksURL        string
	width           int
	height          int
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
	flag.StringVar(&cfg.servicePort, "service-port", "8000", "port of the demo service")
	flag.StringVar(&cfg.localPort, "local-port", "8080", "port the Host opens for the service")
	flag.StringVar(&cfg.hostIP, "host-ip", "127.0.0.2", "address the Host is registered at")
	flag.StringVar(&cfg.hostPort, "host-port", "2222", "SSH port the Host is registered at")
	flag.StringVar(&cfg.hostUser, "host-user", "demo", "SSH user of the Host")
	flag.StringVar(&cfg.hostPassword, "host-password", "demo-host-password", "SSH password of the Host")
	flag.StringVar(&cfg.serviceURL, "service-url", "http://127.0.0.2:8080/", "address the Host opened, as a browser reaches it")
	flag.StringVar(&cfg.forwardPort, "forward-port", "18080", "port the local forward opens on this machine")
	flag.StringVar(&cfg.targetIP, "target-ip", "127.0.0.1", "address the local forward reaches from the Host")
	flag.StringVar(&cfg.targetPort, "target-port", "80", "port the local forward reaches from the Host")
	flag.StringVar(&cfg.socksPort, "socks-port", "1080", "port the SOCKS5 proxy of the Host listens on here")
	flag.StringVar(&cfg.socksURL, "socks-url", "http://127.0.0.1/", "address opened through the SOCKS5 proxy, as the Host reaches it")
	flag.IntVar(&cfg.width, "width", 1440, "viewport width")
	flag.IntVar(&cfg.height, "height", 900, "viewport height")
	flag.Parse()

	if cfg.passwordFile == "" {
		log.Fatal("-initial-password-file is required")
	}

	if err := record(cfg); err != nil {
		log.Fatal(err)
	}
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

	ctx, cancelTimeout := context.WithTimeout(ctx, 6*time.Minute)
	defer cancelTimeout()

	r := &recorder{ctx: ctx, dir: cfg.frames, options: options, width: cfg.width, height: cfg.height}

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
		{"status", sceneStatus},
		{"service", sceneService},
		{"local forward", sceneLocalForward},
		{"status again", sceneStatusBoth},
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

func sceneServicePort(r *recorder, cfg config, _ string) error {
	if err := r.click(`nav a[data-screen="service-ports"]`, `#service-port-create-service_ip`); err != nil {
		return err
	}

	if err := r.caption("2. Add the service to publish, and the port the Hosts open for it"); err != nil {
		return err
	}

	r.pause(time.Second)

	fields := []struct{ sel, text string }{
		{`#service-port-create-service_ip`, cfg.serviceIP},
		{`#service-port-create-service_port`, cfg.servicePort},
		{`#service-port-create-local_port`, cfg.localPort},
		{`#service-port-create-description`, "Demo web service"},
	}

	for _, field := range fields {
		if err := r.typeInto(field.sel, field.text, 3); err != nil {
			return err
		}
	}

	if err := r.click(`[data-action="service-port-create-submit"]`, `[data-action^="service-port-edit-"]`); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	r.pause(1500 * time.Millisecond)

	return nil
}

func sceneHost(r *recorder, cfg config, _ string) error {
	if err := r.click(`nav a[data-screen="hosts"]`, `#host-create-ip`); err != nil {
		return err
	}

	if err := r.caption("3. Add the Host, an SSH server the clients can reach"); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.typeInto(`#host-create-ip`, cfg.hostIP, 3); err != nil {
		return err
	}

	if err := r.run(chromedp.SetValue(`#host-create-port`, "", chromedp.ByQuery)); err != nil {
		return err
	}

	fields := []struct {
		sel, text string
		chunk     int
	}{
		{`#host-create-port`, cfg.hostPort, 2},
		{`#host-create-user`, cfg.hostUser, 2},
		{`#host-create-password`, cfg.hostPassword, 4},
		{`#host-create-description`, "Demo SSH server", 3},
	}

	for _, field := range fields {
		if err := r.typeInto(field.sel, field.text, field.chunk); err != nil {
			return err
		}
	}

	if err := r.click(`[data-action="host-create-submit"]`, `[data-action^="host-edit-"]`); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	r.pause(1500 * time.Millisecond)

	return nil
}

func sceneStatus(r *recorder, _ config, _ string) error {
	if err := r.click(`nav a[data-screen="status"]`, `[data-count="connected"]`); err != nil {
		return err
	}

	if err := r.caption("4. Approve the host key of the Host and the tunnel comes up"); err != nil {
		return err
	}

	if err := r.waitWhileShooting(`[data-action="host-keys"]`, 90*time.Second); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.click(`[data-action="host-keys"]`, `[data-modal-panel="host-keys"] .host-key-row button`); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.click(`[data-modal-panel="host-keys"] .host-key-row button`, `[data-action="host-key-approve"]`); err != nil {
		return err
	}

	r.pause(1500 * time.Millisecond)

	if err := r.click(`[data-action="host-key-approve"]`, ``); err != nil {
		return err
	}

	if err := r.waitGone(`[data-modal-panel="host-key"]`, 10*time.Second); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.click(`[data-action="host-keys-close"]`, ``); err != nil {
		return err
	}

	if err := r.waitWhileShooting(`.badge[data-status="connected"]`, 90*time.Second); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.point(`.badge[data-status="connected"]`); err != nil {
		return err
	}

	if err := r.shot(2500 * time.Millisecond); err != nil {
		return err
	}

	return nil
}

func sceneService(r *recorder, cfg config, _ string) error {
	if err := r.navigate(cfg.serviceURL); err != nil {
		return err
	}

	if err := r.run(
		chromedp.WaitVisible(`h1`, chromedp.ByQuery),
	); err != nil {
		return err
	}

	var heading string

	if err := r.run(chromedp.Text(`h1`, &heading, chromedp.ByQuery)); err != nil {
		return err
	}

	if !strings.Contains(heading, "Hello from the service behind the tunnel") {
		return fmt.Errorf("the page at %s says %q and not the demo service", cfg.serviceURL, heading)
	}

	if err := r.caption("5. A client opens the port on the Host and reaches the service"); err != nil {
		return err
	}

	return r.shot(3500 * time.Millisecond)
}

func sceneLocalForward(r *recorder, cfg config, _ string) error {
	if err := r.navigate(cfg.base + "/ui/hosts"); err != nil {
		return err
	}

	if err := r.run(chromedp.WaitVisible(`[data-action^="host-local-forwards-"]`, chromedp.ByQuery)); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.caption("6. Open a port here that reaches a service only the Host can reach"); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.click(`[data-action^="host-local-forwards-"]`, `[data-action="local-forward-add"]`); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.click(`[data-action="local-forward-add"]`, `#local-forward-create-local_port`); err != nil {
		return err
	}

	r.pause(time.Second)

	fields := []struct{ sel, text string }{
		{`#local-forward-create-local_port`, cfg.forwardPort},
		{`#local-forward-create-target_ip`, cfg.targetIP},
		{`#local-forward-create-target_port`, cfg.targetPort},
		{`#local-forward-create-description`, "Web inside the Host"},
	}

	for _, field := range fields {
		if err := r.typeInto(field.sel, field.text, 3); err != nil {
			return err
		}
	}

	connected := `[data-modal-panel="local-forwards"] .badge[data-status="connected"]`

	if err := r.click(`[data-action="local-forward-create-submit"]`, ``); err != nil {
		return err
	}

	if err := r.waitGone(`[data-modal-panel="local-forward-add"]`, 10*time.Second); err != nil {
		return err
	}

	if err := r.waitWhileShooting(connected, 60*time.Second); err != nil {
		return err
	}

	if err := r.point(connected); err != nil {
		return err
	}

	if err := r.shot(2500 * time.Millisecond); err != nil {
		return err
	}

	if err := r.click(`[data-action="local-forwards-close"]`, ``); err != nil {
		return err
	}

	address := fmt.Sprintf("http://127.0.0.1:%s/", cfg.forwardPort)

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

	if err := r.caption("7. This machine opens the port and reaches the web server inside the Host"); err != nil {
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

	if err := r.caption("8. The status screen holds the tunnel and the local forward in one table"); err != nil {
		return err
	}

	// Both sorts of row carry the same badge, so what is waited for is the
	// second one rather than a badge of its own.
	both := `document.querySelectorAll('table tbody .badge[data-status="connected"]').length >= 2`

	if err := r.waitTrueWhileShooting(both, "both rows to be connected", 60*time.Second); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	return r.shot(3500 * time.Millisecond)
}

func sceneSOCKS(r *recorder, cfg config, _ string) error {
	if err := r.navigate(cfg.base + "/ui/hosts"); err != nil {
		return err
	}

	if err := r.run(chromedp.WaitVisible(`[data-action^="host-edit-"]`, chromedp.ByQuery)); err != nil {
		return err
	}

	if err := r.scrollTo(`table`); err != nil {
		return err
	}

	if err := r.caption("9. Turn on a SOCKS5 proxy and browse the network behind the Host"); err != nil {
		return err
	}

	r.pause(time.Second)

	if err := r.click(`[data-action^="host-edit-"]`, `#host-edit-socks_enabled`); err != nil {
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

// throughProxy opens cfg.socksURL in a second Chrome that sends everything,
// loopback addresses included, through the SOCKS5 proxy, and takes its frames
// into the same recording.
func (r *recorder) throughProxy(cfg config) error {
	options := append([]chromedp.ExecAllocatorOption{}, r.options...)
	options = append(options,
		chromedp.ProxyServer("socks5://127.0.0.1:"+cfg.socksPort),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
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

	if err := r.caption("10. A browser set to the SOCKS5 proxy opens the address as the Host sees it"); err != nil {
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
		r.frames[len(r.frames)-1].hold += d
	}
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
	if err := r.run(chromedp.WaitVisible(sel, chromedp.ByQuery)); err != nil {
		return err
	}

	if err := r.point(sel); err != nil {
		return err
	}

	if err := r.run(chromedp.Focus(sel, chromedp.ByQuery)); err != nil {
		return err
	}

	runes := []rune(text)

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
	if err := r.run(chromedp.WaitVisible(sel, chromedp.ByQuery)); err != nil {
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
