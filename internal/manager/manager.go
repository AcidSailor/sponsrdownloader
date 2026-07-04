package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/acidsailor/sponsrdownloader/internal/configuration"
	"github.com/acidsailor/sponsrdownloader/pkg/sponsr"
	"github.com/playwright-community/playwright-go"
)

const (
	kinescopeDomain  = "kinescope.io"
	kinescopeReferer = "https://" + kinescopeDomain + "/\r\n"
	videoSelector    = "video"
	jsVideoPlay      = "document.querySelector('" + videoSelector + "').play()"
	jsScrollPage     = `async () => {
		await new Promise(resolve => {
			let pos = 0;
			const step = 300;
			const id = setInterval(() => {
				window.scrollBy(0, step);
				pos += step;
				if (pos >= document.body.scrollHeight) {
					window.scrollTo(0, 0);
					clearInterval(id);
					resolve();
				}
			}, 50);
		});
	}`
	m3u8WaitTimeout = 30 * time.Second
	m3u8Suffix      = ".m3u8"
)

var ErrManager = errors.New("manager")

// Compile-time assertion that *sponsr.Post satisfies Downloadable.
var _ Downloadable = (*sponsr.Post)(nil)

type Downloadable interface {
	URL() string
	Filename() string
	IsAvailable() bool
	UpdatedAt() time.Time
}

type Manager struct {
	browserContext playwright.BrowserContext
	outputPath     string
	ffmpegPath     string
	ffmpegTimeout  time.Duration
	// pw and browser are stored solely for Close().
	pw      *playwright.Playwright
	browser playwright.Browser
}

func NewManager(
	config configuration.Globals,
	projectTitle string,
) (*Manager, error) {
	m, err := newManager(config, projectTitle)
	if err != nil {
		return nil, errors.Join(ErrManager, err)
	}
	slog.Info("created output folder", "path", m.outputPath)
	return m, nil
}

func newManager(
	config configuration.Globals,
	projectTitle string,
) (_ *Manager, err error) {
	// Root downloads under the configurable OutputDir (default: cwd) rather
	// than always the process cwd.
	outputPath := filepath.Join(config.OutputDir, projectTitle)
	if err = os.MkdirAll(outputPath, 0o755); err != nil {
		return nil, fmt.Errorf(
			"could not create output directory %q: %w",
			outputPath,
			err,
		)
	}

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found in PATH: %w", err)
	}

	playwrightOpts := &playwright.RunOptions{
		Browsers: []string{"chromium"},
		Verbose:  false,
	}
	slog.Info("installing playwright chromium (skipped if already installed)")
	if err = playwright.Install(playwrightOpts); err != nil {
		return nil, fmt.Errorf("could not install playwright: %w", err)
	}

	pw, err := playwright.Run(playwrightOpts)
	if err != nil {
		return nil, fmt.Errorf("could not start playwright: %w", err)
	}
	defer func() {
		if err != nil {
			if stopErr := pw.Stop(); stopErr != nil {
				slog.Error("could not stop playwright", "error", stopErr)
			}
		}
	}()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Args: []string{"--no-sandbox"},
	})
	if err != nil {
		return nil, fmt.Errorf("could not launch browser: %w", err)
	}
	defer func() {
		if err != nil {
			if closeErr := browser.Close(); closeErr != nil {
				slog.Error("could not close browser", "error", closeErr)
			}
		}
	}()

	browserContext, err := browser.NewContext()
	if err != nil {
		return nil, fmt.Errorf("could not create browser context: %w", err)
	}
	defer func() {
		if err != nil {
			if closeErr := browserContext.Close(); closeErr != nil {
				slog.Error("could not close browser context", "error", closeErr)
			}
		}
	}()

	err = browserContext.AddCookies([]playwright.OptionalCookie{{
		Name:   config.SessionCookieName,
		Value:  config.SessionCookieValue,
		Domain: new("." + sponsr.Domain),
		Path:   new("/"),
	}})
	if err != nil {
		return nil, fmt.Errorf("could not set cookie: %w", err)
	}

	return &Manager{
		browserContext: browserContext,
		outputPath:     outputPath,
		ffmpegPath:     ffmpegPath,
		ffmpegTimeout:  config.FFmpegTimeout,
		pw:             pw,
		browser:        browser,
	}, nil
}

// Close shuts down the browser context, browser, and Playwright process.
// It must be called (via defer) to release all resources.
func (m *Manager) Close() {
	if err := m.browserContext.Close(); err != nil {
		slog.Error("could not close browser context", "error", err)
	}
	if err := m.browser.Close(); err != nil {
		slog.Error("could not close browser", "error", err)
	}
	if err := m.pw.Stop(); err != nil {
		slog.Error("could not stop playwright", "error", err)
	}
}

// downloadReq describes one output format: the file extension (also
// the noun used in logs/errors) and the function that writes the file
// to the path handed to it.
type downloadReq struct {
	ext          string
	downloadFunc func(context.Context, Downloadable, string) error
}

// download runs the skip-check, the download func, and the sidecar
// write for one item. The output path is built here, once, and handed
// to req.downloadFunc so the file it writes is exactly the file the
// skip/checksum logic tracks.
func (m *Manager) download(
	ctx context.Context,
	item Downloadable,
	req downloadReq,
) error {
	outputPath := filepath.Join(
		m.outputPath, item.Filename()+"."+req.ext)
	logger := slog.With("filename", item.Filename())
	want := sidecar{UpdatedAt: item.UpdatedAt()}

	if want.UpdatedAt.IsZero() {
		logger.Warn("no updated_at from API; edit-detection disabled")
	}
	done, err := want.isCurrent(outputPath)
	if err != nil {
		logger.Warn("could not check existing download", "error", err)
	}
	if done {
		logger.Info("skipped, already downloaded")
		return nil
	}

	if err := req.downloadFunc(ctx, item, outputPath); err != nil {
		// Both %w keep the sentinel and the cause unwrappable, so
		// errors.Is finds ErrManager and the underlying error.
		return fmt.Errorf("%w: %s %q: %w",
			ErrManager, req.ext, item.Filename(), err)
	}
	if err := want.record(outputPath); err != nil {
		return fmt.Errorf(
			"%w: record %s %q: %w",
			ErrManager, req.ext, item.Filename(), err)
	}

	logger.Info("downloaded " + req.ext)
	return nil
}

func (m *Manager) DownloadPDF(
	ctx context.Context,
	item Downloadable,
) error {
	return m.download(ctx, item, downloadReq{
		ext:          "pdf",
		downloadFunc: m.downloadPDF,
	})
}

func (m *Manager) newPage(
	ctx context.Context,
	logger *slog.Logger,
	targetURL string,
) (playwright.Page, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	page, err := m.browserContext.NewPage()
	if err != nil {
		return nil, fmt.Errorf("could not create page: %w", err)
	}
	pageGotoOpts := playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	}
	_, err = page.Goto(targetURL, pageGotoOpts)
	if err != nil {
		if closeErr := page.Close(); closeErr != nil {
			logger.Error("could not close page", "error", closeErr)
		}
		return nil, fmt.Errorf("could not navigate to %s: %w", targetURL, err)
	}
	return page, nil
}

func (m *Manager) downloadPDF(
	ctx context.Context,
	item Downloadable,
	filePath string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	logger := slog.With("filename", item.Filename())
	if !item.IsAvailable() {
		logger.Warn("skipped unavailable item")
		return nil
	}

	logger.Info("downloading PDF")

	page, err := m.newPage(ctx, logger, item.URL())
	if err != nil {
		return err
	}
	defer func() {
		if err := page.Close(); err != nil {
			logger.Error("could not close page", "error", err)
		}
	}()

	// Scroll through the full page to trigger lazy-loaded images.
	if _, err = page.Evaluate(jsScrollPage); err != nil {
		return fmt.Errorf("could not scroll page: %w", err)
	}
	pageWaitOpts := playwright.PageWaitForLoadStateOptions{
		State: playwright.LoadStateNetworkidle,
	}
	if err = page.WaitForLoadState(pageWaitOpts); err != nil {
		return fmt.Errorf(
			"could not wait for network idle after scroll: %w",
			err,
		)
	}

	_, err = page.PDF(playwright.PagePdfOptions{Path: new(filePath)})
	if err != nil {
		return fmt.Errorf("could not create PDF: %w", err)
	}

	return nil
}

func (m *Manager) DownloadVideo(
	ctx context.Context,
	item Downloadable,
) error {
	return m.download(ctx, item, downloadReq{
		ext:          "mp4",
		downloadFunc: m.downloadVideo,
	})
}

func (m *Manager) downloadVideo(
	ctx context.Context,
	item Downloadable,
	filePath string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	logger := slog.With("filename", item.Filename())
	if !item.IsAvailable() {
		logger.Warn("skipped unavailable item")
		return nil
	}

	logger.Info("downloading video")

	page, err := m.browserContext.NewPage()
	if err != nil {
		return fmt.Errorf("could not create page: %w", err)
	}
	defer func() {
		if err := page.Close(); err != nil {
			logger.Error("could not close page", "error", err)
		}
	}()

	m3u8Chan := make(chan string, 1)
	page.OnRequest(func(req playwright.Request) {
		if strings.Contains(req.URL(), m3u8Suffix) {
			select {
			case m3u8Chan <- req.URL():
			default:
			}
		}
	})

	pageGotoOpts := playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	}
	if _, err = page.Goto(item.URL(), pageGotoOpts); err != nil {
		return fmt.Errorf("could not navigate to %s: %w", item.URL(), err)
	}

	var kinescopeFrame playwright.Frame
	for _, f := range page.Frames() {
		if strings.Contains(f.URL(), kinescopeDomain) {
			kinescopeFrame = f
			break
		}
	}
	if kinescopeFrame == nil {
		return fmt.Errorf("no kinescope frame found on %s", item.URL())
	}

	if err = kinescopeFrame.Locator(videoSelector).WaitFor(); err != nil {
		return fmt.Errorf(
			"could not find video element in kinescope frame: %w",
			err,
		)
	}
	if _, err = kinescopeFrame.Evaluate(jsVideoPlay); err != nil {
		return fmt.Errorf("could not trigger video playback: %w", err)
	}

	// Wait for the M3U8 URL to appear in network traffic (bounded to m3u8WaitTimeout).
	m3u8Ctx, m3u8Cancel := context.WithTimeout(ctx, m3u8WaitTimeout)
	defer m3u8Cancel()

	var m3u8URL string
	select {
	case m3u8URL = <-m3u8Chan:
	case <-m3u8Ctx.Done():
		return m3u8Ctx.Err()
	}

	// Prefix relative paths with "./" so ffmpeg can't read a leading dash as
	// a flag; absolute paths are already unambiguous.
	if !filepath.IsAbs(filePath) {
		filePath = "./" + filePath
	}

	// Use a separate, longer timeout for the ffmpeg download phase, derived from parent ctx.
	ffmpegCtx, ffmpegCancel := context.WithTimeout(ctx, m.ffmpegTimeout)
	defer ffmpegCancel()

	cmd := exec.CommandContext(
		ffmpegCtx,
		m.ffmpegPath,
		"-headers", "Referer: "+kinescopeReferer,
		"-i", m3u8URL,
		"-c", "copy",
		"-y",
		filePath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Error("ffmpeg output", "output", string(out))
		return fmt.Errorf("ffmpeg failed: %w", err)
	}

	return nil
}
