package browser

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

const (
	navTimeout        = 60 * time.Second
	networkIdleBudget = 5 * time.Second
	elementWaitBudget = 25 * time.Second
	defaultSettleMs   = 1000
	screenshotMaxJPEG = 90
)

// stealthScript hides the most common headless/automation tells. Lifted from
// the harness browser tool.
const stealthScript = `(function(){
  try { Object.defineProperty(navigator, 'webdriver', { get: () => undefined }); } catch (e) {}
  try { if (!navigator.languages || navigator.languages.length === 0) { Object.defineProperty(navigator, 'languages', { get: () => ['en-US','en'] }); } } catch (e) {}
  try { Object.defineProperty(navigator, 'plugins', { get: () => [1,2,3,4,5] }); } catch (e) {}
  if (!window.chrome) { window.chrome = { runtime: {} }; }
})();`

// ensureChrome lazily connects a chromedp context to the session's CDP endpoint
// (remote allocator) the first time an action runs. Must be called with s.mu held.
func ensureChrome(s *Session) error {
	// Fast path: already connected. A dead taskCtx surfaces its error rather
	// than being recreated — unlike the harness's relaunch, recovery is
	// pointless here because the backing container is gone; the caller closes
	// and creates a fresh browser instead.
	if s.taskCtx != nil {
		return s.taskCtx.Err()
	}
	if s.Endpoint == "" {
		return fmt.Errorf("session has no CDP endpoint")
	}
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), s.Endpoint)
	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	if err := chromedp.Run(taskCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			if _, err := page.AddScriptToEvaluateOnNewDocument(stealthScript).Do(ctx); err != nil {
				return err
			}
			return page.SetLifecycleEventsEnabled(true).Do(ctx)
		}),
	); err != nil {
		taskCancel()
		allocCancel()
		return err
	}
	s.taskCtx = taskCtx
	s.cancel = func() { taskCancel(); allocCancel() }
	return nil
}

// withAction runs fn against the session's chromedp ctx under a per-call
// deadline, holding s.mu (one tab, serialized). ensureChrome runs first.
func withAction(parent context.Context, s *Session, timeout time.Duration, fn func(ctx context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureChrome(s); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.taskCtx, timeout)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-parent.Done():
			cancel()
		case <-done:
		}
	}()
	return fn(ctx)
}

// Navigate loads url and waits for readiness, updating s.CurrentURL on success.
func Navigate(parent context.Context, s *Session, url, waitFor string, waitMs int) (title string, err error) {
	err = withAction(parent, s, navTimeout, func(ctx context.Context) error {
		idleCh := make(chan struct{}, 1)
		chromedp.ListenTarget(ctx, func(ev any) {
			if e, ok := ev.(*page.EventLifecycleEvent); ok && e.Name == "networkIdle" {
				select {
				case idleCh <- struct{}{}:
				default:
				}
			}
		})
		if err := chromedp.Run(ctx, chromedp.Navigate(url), chromedp.WaitReady("body")); err != nil {
			return err
		}
		select {
		case <-idleCh:
		case <-time.After(networkIdleBudget):
		case <-ctx.Done():
			return ctx.Err()
		}
		if waitFor != "" {
			wc, wcancel := context.WithTimeout(ctx, elementWaitBudget)
			defer wcancel()
			if err := chromedp.Run(wc, chromedp.WaitVisible(waitFor)); err != nil {
				return fmt.Errorf("wait_for %q: %w", waitFor, err)
			}
		}
		settle := waitMs
		if settle <= 0 {
			settle = defaultSettleMs
		}
		if err := chromedp.Run(ctx, chromedp.Sleep(time.Duration(settle)*time.Millisecond)); err != nil {
			return err
		}
		if err := chromedp.Run(ctx, chromedp.Title(&title)); err != nil {
			return err
		}
		s.CurrentURL = url // under s.mu (withAction holds it)
		return nil
	})
	return title, err
}

// Click clicks the selector (waiting for it to be visible).
func Click(parent context.Context, s *Session, selector, waitFor string) error {
	return withAction(parent, s, navTimeout, func(ctx context.Context) error {
		sel := selector
		if waitFor != "" {
			sel = waitFor
		}
		if err := waitVisible(ctx, sel); err != nil {
			return err
		}
		return chromedp.Run(ctx, chromedp.Click(selector))
	})
}

// TypeText clears the selector and types text into it.
func TypeText(parent context.Context, s *Session, selector, text string) error {
	return withAction(parent, s, navTimeout, func(ctx context.Context) error {
		if err := waitVisible(ctx, selector); err != nil {
			return err
		}
		return chromedp.Run(ctx, chromedp.Clear(selector), chromedp.SendKeys(selector, text))
	})
}

// GetHTML returns the innerHTML of selector (or body).
func GetHTML(parent context.Context, s *Session, selector string) (string, error) {
	if selector == "" {
		selector = "body"
	}
	var out string
	err := withAction(parent, s, navTimeout, func(ctx context.Context) error {
		if err := waitReady(ctx, selector); err != nil {
			return err
		}
		return chromedp.Run(ctx, chromedp.InnerHTML(selector, &out))
	})
	return out, err
}

// Screenshot returns a full-page JPEG.
func Screenshot(parent context.Context, s *Session) ([]byte, error) {
	var buf []byte
	err := withAction(parent, s, navTimeout, func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.FullScreenshot(&buf, screenshotMaxJPEG))
	})
	return buf, err
}

// Evaluate runs script and returns the JSON-able result.
func Evaluate(parent context.Context, s *Session, script string) (any, error) {
	var result any
	err := withAction(parent, s, navTimeout, func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Evaluate(script, &result))
	})
	return result, err
}

func waitVisible(ctx context.Context, selector string) error {
	wc, cancel := context.WithTimeout(ctx, elementWaitBudget)
	defer cancel()
	if err := chromedp.Run(wc, chromedp.WaitVisible(selector)); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timed out waiting for selector %q (CSS only — Playwright :has-text()/text= are unsupported)", selector)
		}
		return fmt.Errorf("wait %q: %w", selector, err)
	}
	return nil
}

func waitReady(ctx context.Context, selector string) error {
	wc, cancel := context.WithTimeout(ctx, elementWaitBudget)
	defer cancel()
	if err := chromedp.Run(wc, chromedp.WaitReady(selector)); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timed out waiting for selector %q", selector)
		}
		return fmt.Errorf("wait %q: %w", selector, err)
	}
	return nil
}
