// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package screentest implements script-based visual diff testing
// for webpages.
//
// Scripts
//
// A script is a template file containing a sequence of testcases, separated by
// blank lines. Lines beginning with # characters are ignored as comments. A
// testcase is a sequence of lines describing actions to take on a page, along
// with the dimensions of the screenshots to be compared. For example, here is
// a trivial script:
//
//  compare https://go.dev {{.ComparisonURL}}
//  pathname /about
//  capture fullscreen
//
// This script has a single testcase. The first line sets the origin servers to
// compare. The second line sets the page to visit at each origin. The last line
// captures fullpage screenshots of the pages and generates a diff image if they
// do not match.
//
// Keywords
//
// Use windowsize WIDTHxHEIGHT to set the default window size for all testcases
// that follow.
//
//  windowsize 540x1080
//
// Use compare ORIGIN ORIGIN to set the origins to compare.
//
//  compare https://go.dev http://localhost:6060
//
// Use header KEY:VALUE to add headers to requests
//
//  header Authorization: Bearer token
//
// Add the ::cache suffix to cache the images from an origin for subsequent
// test runs.
//
//  compare https://go.dev::cache http://localhost:6060
//
// Use output DIRECTORY to set the output directory for diffs and cached images.
//
//  output testdata/snapshots
//
// USE output BUCKETNAME for screentest to upload test output to a Cloud Storage bucket.
// The bucket must already exist prior to running the tests.
//
//  output gs://bucket-name
//
// Use test NAME to create a name for the testcase.
//
//  test about page
//
// Use pathname PATH to set the page to visit at each origin. If no
// test name is set, PATH will be used as the name for the test.
//
//  pathname /about
//
// Use click SELECTOR to add a click an element on the page.
//
//  click button.submit
//
// Use wait SELECTOR to wait for an element to appear.
//
//  wait [role="treeitem"][aria-expanded="true"]
//
// Use capture [SIZE] [ARG] to create a testcase with the properties
// defined above.
//
//  capture fullscreen 540x1080
//
// When taking an element screenshot provide a selector.
//
//  capture element header
//
// Chain capture commands to create multiple testcases for a single page.
//
//  windowsize 1536x960
//  compare https://go.dev::cache http://localhost:6060
//  output testdata/snapshots
//
//  test homepage
//  pathname /
//  capture viewport
//  capture viewport 540x1080
//  capture viewport 400x1000
//
//  test about page
//  pathname /about
//  capture viewport
//  capture viewport 540x1080
//  capture viewport 400x1000
//
package screentest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	"cloud.google.com/go/storage"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/n7olkachev/imgdiff/pkg/imgdiff"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/iterator"
)

// CheckHandler runs the test scripts matched by glob. If any errors are
// encountered, CheckHandler returns an error listing the problems.
func CheckHandler(glob string, update bool, vars map[string]string) error {
	now := time.Now()
	ctx := context.Background()
	files, err := filepath.Glob(glob)
	if err != nil {
		return fmt.Errorf("filepath.Glob(%q): %w", glob, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no files match %q", glob)
	}
	ctx, cancel := chromedp.NewExecAllocator(ctx, append(
		chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(browserWidth, browserHeight),
	)...)
	defer cancel()
	var buf bytes.Buffer
	for _, file := range files {
		tests, err := readTests(file, vars)
		if err != nil {
			return fmt.Errorf("readTestdata(%q): %w", file, err)
		}
		if len(tests) == 0 {
			return fmt.Errorf("no tests found in %q", file)
		}
		if err := cleanOutput(ctx, tests); err != nil {
			return fmt.Errorf("cleanOutput(...): %w", err)
		}
		ctx, cancel = chromedp.NewContext(ctx, chromedp.WithLogf(log.Printf))
		defer cancel()
		var hdr bool
		for _, test := range tests {
			if err := runDiff(ctx, test, update); err != nil {
				if !hdr {
					fmt.Fprintf(&buf, "%s\n\n", file)
					hdr = true
				}
				fmt.Fprintf(&buf, "%v\n", err)
				fmt.Fprintf(&buf, "inspect diff at %s\n\n", test.outDiff)
			}
		}
	}
	fmt.Printf("finished in %s\n\n", time.Since(now).Truncate(time.Millisecond))
	if buf.Len() > 0 {
		return errors.New(buf.String())
	}
	return nil
}

// TestHandler runs the test script files matched by glob.
func TestHandler(t *testing.T, glob string, update bool, vars map[string]string) {
	ctx := context.Background()
	files, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal(fmt.Errorf("no files match %#q", glob))
	}
	ctx, cancel := chromedp.NewExecAllocator(ctx, append(
		chromedp.DefaultExecAllocatorOptions[:],
		chromedp.WindowSize(browserWidth, browserHeight),
	)...)
	defer cancel()
	for _, file := range files {
		tests, err := readTests(file, vars)
		if err != nil {
			t.Fatal(err)
		}
		if err := cleanOutput(ctx, tests); err != nil {
			t.Fatal(err)
		}
		ctx, cancel = chromedp.NewContext(ctx, chromedp.WithLogf(t.Logf))
		defer cancel()
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if err := runDiff(ctx, test, update); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// cleanOutput clears the output locations of images not cached
// as part of a testcase, including diff output from previous test
// runs and obsolete screenshots. It ensures local directories exist
// for test output. GCS buckets must already exist prior to test run.
func cleanOutput(ctx context.Context, tests []*testcase) error {
	keepFiles := make(map[string]bool)
	bkts := make(map[string]bool)
	dirs := make(map[string]bool)
	// The extensions of files that are safe to delete
	safeExts := map[string]bool{
		"diff.png": true,
	}
	for _, t := range tests {
		if t.cacheA {
			keepFiles[t.outImgA] = true
			safeExts[ext(t.outImgA)] = true
		}
		if t.cacheB {
			keepFiles[t.outImgB] = true
			safeExts[ext(t.outImgB)] = true
		}
		if t.gcsBucket {
			bkt, _ := gcsParts(t.outDiff)
			bkts[bkt] = true
		} else {
			dirs[filepath.Dir(t.outDiff)] = true
		}
	}
	if err := cleanBkts(ctx, bkts, keepFiles, safeExts); err != nil {
		return fmt.Errorf("cleanBkts(...): %w", err)
	}
	if err := cleanDirs(dirs, keepFiles, safeExts); err != nil {
		return fmt.Errorf("cleanDirs(...): %w", err)
	}
	return nil
}

// cleanBkts clears all the GCS buckets in bkts of all objects not included
// in the set of keepFiles. Buckets that do not exist will cause an error.
func cleanBkts(ctx context.Context, bkts, keepFiles, safeExts map[string]bool) error {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("storage.NewClient(ctx): %w", err)
	}
	defer client.Close()
	for bkt := range bkts {
		it := client.Bucket(bkt).Objects(ctx, nil)
		for {
			attrs, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return fmt.Errorf("it.Next(): %w", err)
			}
			filename := "gs://" + attrs.Bucket + "/" + attrs.Name
			if !keepFiles[filename] && safeExts[ext(filename)] {
				if err := client.Bucket(attrs.Bucket).Object(attrs.Name).Delete(ctx); err != nil &&
					!errors.Is(err, storage.ErrObjectNotExist) {
					return fmt.Errorf("Object(%q).Delete: %v", attrs.Name, err)
				}
			}
		}
	}
	return client.Close()
}

// cleanBkts ensures the set of directories in dirs exists and
// clears dirs of all files not included in the set of keepFiles.
func cleanDirs(dirs, keepFiles, safeExts map[string]bool) error {
	for dir := range dirs {
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return fmt.Errorf("os.MkdirAll(%q): %w", dir, err)
		}
	}
	for dir := range dirs {
		files, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("os.ReadDir(%q): %w", dir, err)
		}
		for _, f := range files {
			filename := dir + "/" + f.Name()
			if !keepFiles[filename] && safeExts[ext(filename)] {
				if err := os.Remove(filename); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("os.Remove(%q): %w", filename, err)
				}
			}
		}
	}
	return nil
}

func ext(filename string) string {
	// If the filename has multiple dots use the first one as
	// the split for the extension.
	if strings.Count(filename, ".") > 1 {
		base := filepath.Base(filename)
		parts := strings.SplitN(base, ".", 2)
		return parts[1]
	}
	return filepath.Ext(filename)
}

const (
	browserWidth  = 1536
	browserHeight = 960
	cacheSuffix   = "::cache"
	gcsScheme     = "gs://"
)

var sanitize = regexp.MustCompile("[.*<>?`'|/\\: ]")

type screenshotType int

const (
	fullScreenshot screenshotType = iota
	viewportScreenshot
	elementScreenshot
)

type testcase struct {
	name                      string
	tasks                     chromedp.Tasks
	urlA, urlB                string
	headers                   map[string]interface{}
	cacheA, cacheB            bool
	gcsBucket                 bool
	outImgA, outImgB, outDiff string
	viewportWidth             int
	viewportHeight            int
	screenshotType            screenshotType
	screenshotElement         string
}

func (t *testcase) String() string {
	return t.name
}

// readTests parses the testcases from a text file.
func readTests(file string, vars map[string]string) ([]*testcase, error) {
	tmpl, err := template.ParseFiles(file)
	if err != nil {
		return nil, fmt.Errorf("template.ParseFiles(%q): %w", file, err)
	}
	var tmplout bytes.Buffer
	if err := tmpl.Execute(&tmplout, vars); err != nil {
		return nil, fmt.Errorf("tmpl.Execute(...): %w", err)
	}
	var tests []*testcase
	var (
		testName, pathname string
		tasks              chromedp.Tasks
		originA, originB   string
		headers            map[string]interface{}
		cacheA, cacheB     bool
		gcsBucket          bool
		width, height      int
		lineNo             int
	)
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("os.UserCacheDir(): %w", err)
	}
	dir := filepath.Join(cache, "screentest")
	out := outDir(dir, file)
	scan := bufio.NewScanner(&tmplout)
	for scan.Scan() {
		lineNo += 1
		line := strings.TrimSpace(scan.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimRight(line, " \t")
		field, args := splitOneField(line)
		field = strings.ToUpper(field)
		switch field {
		case "":
			// We've reached an empty line, reset properties scoped to a single test.
			testName = ""
			pathname = ""
			tasks = nil
		case "COMPARE":
			origins := strings.Split(args, " ")
			originA, originB = origins[0], origins[1]
			cacheA, cacheB = false, false
			if headers != nil {
				headers = make(map[string]interface{})
			}
			if strings.HasSuffix(originA, cacheSuffix) {
				originA = strings.TrimSuffix(originA, cacheSuffix)
				cacheA = true
			}
			if strings.HasSuffix(originB, cacheSuffix) {
				originB = strings.TrimSuffix(originB, cacheSuffix)
				cacheB = true
			}
			if _, err := url.Parse(originA); err != nil {
				return nil, fmt.Errorf("url.Parse(%q): %w", originA, err)
			}
			if _, err := url.Parse(originB); err != nil {
				return nil, fmt.Errorf("url.Parse(%q): %w", originB, err)
			}
		case "HEADER":
			if headers == nil {
				headers = make(map[string]interface{})
			}
			parts := strings.SplitN(args, ":", 2)
			if len(parts) != 2 {
				log.Fatalf("invalid header %s on line %d", args, lineNo)
			}
			headers[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		case "OUTPUT":
			if strings.HasPrefix(args, gcsScheme) {
				gcsBucket = true
			}
			out = outDir(args, "")
		case "WINDOWSIZE":
			width, height, err = splitDimensions(args)
			if err != nil {
				return nil, fmt.Errorf("splitDimensions(%q): %w", args, err)
			}
		case "TEST":
			testName = args
			for _, t := range tests {
				if t.name == testName {
					return nil, fmt.Errorf(
						"duplicate test name %q on line %d", testName, lineNo)
				}
			}
		case "PATHNAME":
			if _, err := url.Parse(originA + args); err != nil {
				return nil, fmt.Errorf("url.Parse(%q): %w", originA+args, err)
			}
			if _, err := url.Parse(originB + args); err != nil {
				return nil, fmt.Errorf("url.Parse(%q): %w", originB+args, err)
			}
			pathname = args
			if testName == "" {
				testName = pathname[1:]
			}
			for _, t := range tests {
				if t.name == testName {
					return nil, fmt.Errorf(
						"duplicate test with pathname %q on line %d", pathname, lineNo)
				}
			}
		case "CLICK":
			tasks = append(tasks, chromedp.Click(args))
		case "WAIT":
			tasks = append(tasks, chromedp.WaitReady(args))
		case "EVAL":
			tasks = append(tasks, chromedp.Evaluate(args, nil))
		case "CAPTURE":
			if originA == "" || originB == "" {
				return nil, fmt.Errorf("missing compare for capture on line %d", lineNo)
			}
			if pathname == "" {
				return nil, fmt.Errorf("missing pathname for capture on line %d", lineNo)
			}
			urlA, err := url.Parse(originA + pathname)
			if err != nil {
				return nil, fmt.Errorf("url.Parse(%q): %w", originA+pathname, err)
			}
			urlB, err := url.Parse(originB + pathname)
			if err != nil {
				return nil, fmt.Errorf("url.Parse(%q): %w", originB+pathname, err)
			}
			test := &testcase{
				name:    testName,
				tasks:   tasks,
				urlA:    urlA.String(),
				urlB:    urlB.String(),
				headers: headers,
				// Default to viewportScreenshot
				screenshotType: viewportScreenshot,
				viewportWidth:  width,
				viewportHeight: height,
				cacheA:         cacheA,
				cacheB:         cacheB,
				gcsBucket:      gcsBucket,
			}
			tests = append(tests, test)
			field, args := splitOneField(args)
			field = strings.ToUpper(field)
			switch field {
			case "FULLSCREEN", "VIEWPORT":
				if field == "FULLSCREEN" {
					test.screenshotType = fullScreenshot
				}
				if args != "" {
					w, h, err := splitDimensions(args)
					if err != nil {
						return nil, fmt.Errorf("splitDimensions(%q): %w", args, err)
					}
					test.name += fmt.Sprintf(" %dx%d", w, h)
					test.viewportWidth = w
					test.viewportHeight = h
				}
			case "ELEMENT":
				test.name += fmt.Sprintf(" %s", args)
				test.screenshotType = elementScreenshot
				test.screenshotElement = args
			}
			outfile := filepath.Join(out, sanitized(test.name))
			if gcsBucket {
				outfile = out + "/" + sanitized(test.name)
			}
			test.outImgA = outfile + "." + sanitized(urlA.Host) + ".png"
			test.outImgB = outfile + "." + sanitized(urlB.Host) + ".png"
			test.outDiff = outfile + ".diff.png"
		default:
			// We should never reach this error.
			return nil, fmt.Errorf("invalid syntax on line %d: %q", lineNo, line)
		}
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("scan.Err(): %v", err)
	}
	return tests, nil
}

// outDir gets a diff output directory for a given testfile.
// If dir points to a GCS bucket or testfile is empty it just
// returns dir.
func outDir(dir, testfile string) string {
	if strings.HasPrefix(dir, gcsScheme) {
		return dir
	}
	if testfile != "" {
		dir = filepath.Join(dir, sanitized(filepath.Base(testfile)))
	}
	return filepath.Clean(dir)
}

// splitOneField splits text at the first space or tab
// and returns that first field and the remaining text.
func splitOneField(text string) (field, rest string) {
	i := strings.IndexAny(text, " \t")
	if i < 0 {
		return text, ""
	}
	return text[:i], strings.TrimLeft(text[i:], " \t")
}

// splitDimensions parses a window dimension string into int values
// for width and height.
func splitDimensions(text string) (width, height int, err error) {
	windowsize := strings.Split(text, "x")
	if len(windowsize) != 2 {
		return width, height, fmt.Errorf("syntax error: windowsize %s", text)
	}
	width, err = strconv.Atoi(windowsize[0])
	if err != nil {
		return width, height, fmt.Errorf("strconv.Atoi(%q): %w", windowsize[0], err)
	}
	height, err = strconv.Atoi(windowsize[1])
	if err != nil {
		return width, height, fmt.Errorf("strconv.Atoi(%q): %w", windowsize[1], err)
	}
	return width, height, nil
}

// runDiff generates screenshots for a given test case and
// a diff if the screenshots do not match.
func runDiff(ctx context.Context, test *testcase, update bool) (err error) {
	now := time.Now()
	fmt.Printf("test %s\n", test.name)
	var screenA, screenB *image.Image
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		screenA, err = screenshot(ctx, test, test.urlA, test.outImgA, test.cacheA, update)
		if err != nil {
			return fmt.Errorf("screenshot(ctx, %q, %q, %q, %v): %w", test, test.urlA, test.outImgA, test.cacheA, err)
		}
		return nil
	})
	g.Go(func() error {
		screenB, err = screenshot(ctx, test, test.urlB, test.outImgB, test.cacheB, update)
		if err != nil {
			return fmt.Errorf("screenshot(ctx, %q, %q, %q, %v): %w", test, test.urlB, test.outImgB, test.cacheB, err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return err
	}
	result := imgdiff.Diff(*screenA, *screenB, &imgdiff.Options{
		Threshold: 0.1,
		DiffImage: true,
	})
	since := time.Since(now).Truncate(time.Millisecond)
	if result.Equal {
		fmt.Printf("%s == %s (%s)\n\n", test.urlA, test.urlB, since)
		return nil
	}
	fmt.Printf("%s != %s (%s)\n", test.urlA, test.urlB, since)
	g = &errgroup.Group{}
	g.Go(func() error {
		return writePNG(test, &result.Image, test.outDiff)
	})
	// Only write screenshots if they haven't already been written to the cache.
	if !test.cacheA {
		g.Go(func() error {
			return writePNG(test, screenA, test.outImgA)
		})
	}
	if !test.cacheB {
		g.Go(func() error {
			return writePNG(test, screenB, test.outImgB)
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("writePNG(...): %w", err)
	}
	fmt.Printf("wrote diff to %s\n\n", test.outDiff)
	return fmt.Errorf("%s != %s", test.urlA, test.urlB)
}

// screenshot gets a screenshot for a testcase url. When cache is true it will
// attempt to read the screenshot from a cache or capture a new screenshot
// and write it to the cache if it does not exist.
func screenshot(ctx context.Context, test *testcase,
	url, file string, cache, update bool,
) (_ *image.Image, err error) {
	var data []byte
	// If cache is enabled, try to read the file from the cache.
	if cache && test.gcsBucket {
		client, err := storage.NewClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("storage.NewClient(err): %w", err)
		}
		defer client.Close()
		bkt, obj := gcsParts(file)
		r, err := client.Bucket(bkt).Object(obj).NewReader(ctx)
		if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			return nil, fmt.Errorf("object.NewReader(ctx): %w", err)
		} else if err == nil {
			defer r.Close()
			data, err = ioutil.ReadAll(r)
			if err != nil {
				return nil, fmt.Errorf("ioutil.ReadAll(...): %w", err)
			}
		}
	} else if cache {
		data, err = os.ReadFile(file)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("os.ReadFile(...): %w", err)
		}
	}
	// If cache is false, an update is requested, or this is the first test run
	// we capture a new screenshot from a live URL.
	if !cache || update || data == nil {
		update = true
		data, err = captureScreenshot(ctx, test, url)
		if err != nil {
			return nil, fmt.Errorf("captureScreenshot(ctx, %q, %q): %w", url, test, err)
		}
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("image.Decode(...): %w", err)
	}
	// Write to the cache.
	if cache && update {
		if err := writePNG(test, &img, file); err != nil {
			return nil, fmt.Errorf("os.WriteFile(...): %w", err)
		}
		fmt.Printf("updated %s\n", file)
	}
	return &img, nil
}

// captureScreenshot runs a series of browser actions and takes a screenshot
// of the resulting webpage in an instance of headless chrome.
func captureScreenshot(ctx context.Context, test *testcase, url string) ([]byte, error) {
	var buf []byte
	ctx, cancel := chromedp.NewContext(ctx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var tasks chromedp.Tasks
	if test.headers != nil {
		tasks = append(tasks, network.SetExtraHTTPHeaders(test.headers))
	}
	tasks = append(tasks,
		chromedp.EmulateViewport(int64(test.viewportWidth), int64(test.viewportHeight)),
		chromedp.Navigate(url),
		waitForEvent("networkIdle"),
		test.tasks,
	)
	switch test.screenshotType {
	case fullScreenshot:
		tasks = append(tasks, chromedp.FullScreenshot(&buf, 100))
	case viewportScreenshot:
		tasks = append(tasks, chromedp.CaptureScreenshot(&buf))
	case elementScreenshot:
		tasks = append(tasks, chromedp.Screenshot(test.screenshotElement, &buf))
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return nil, fmt.Errorf("chromedp.Run(...): %w", err)
	}
	return buf, nil
}

// writePNG writes image data to a png file.
func writePNG(test *testcase, i *image.Image, filename string) (err error) {
	var f io.WriteCloser
	if test.gcsBucket {
		ctx := context.Background()
		client, err := storage.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("storage.NewClient(ctx): %w", err)
		}
		defer client.Close()
		bkt, obj := gcsParts(filename)
		f = client.Bucket(bkt).Object(obj).NewWriter(ctx)
	} else {
		f, err = os.Create(filename)
		if err != nil {
			return fmt.Errorf("os.Create(%q): %w", filename, err)
		}
	}
	err = png.Encode(f, *i)
	if err != nil {
		// Ignore f.Close() error, since png.Encode returned an error.
		_ = f.Close()
		return fmt.Errorf("png.Encode(...): %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("f.Close(): %w", err)
	}
	return nil
}

// sanitized transforms text into a string suitable for use in a
// filename part.
func sanitized(text string) string {
	return sanitize.ReplaceAllString(text, "-")
}

// gcsParts splits a Cloud Storage filename into bucket name and
// object name parts.
func gcsParts(filename string) (bucket, object string) {
	filename = strings.TrimPrefix(filename, gcsScheme)
	n := strings.Index(filename, "/")
	bucket = filename[:n]
	object = filename[n+1:]
	return bucket, object
}

// waitForEvent waits for browser lifecycle events. This is useful for
// ensuring the page is fully loaded before capturing screenshots.
func waitForEvent(eventName string) chromedp.ActionFunc {
	return func(ctx context.Context) error {
		ch := make(chan struct{})
		cctx, cancel := context.WithCancel(ctx)
		chromedp.ListenTarget(cctx, func(ev interface{}) {
			switch e := ev.(type) {
			case *page.EventLifecycleEvent:
				if e.Name == eventName {
					cancel()
					close(ch)
				}
			}
		})
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
