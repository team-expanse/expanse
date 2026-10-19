// Package vm prepares a vm/instance guest: the cloud image written onto its disk on the first
// start, the cloud-init seed it boots with, and its stable MAC address.
package vm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Image is vm/instance's image setting: where to download it and the digest it must have,
// given directly or as a line of a SHA256SUMS/SHA512SUMS file.
type Image struct {
	URL         string
	Checksum    string
	ChecksumURL string
}

var digestRE = regexp.MustCompile(`^([0-9a-fA-F]{64}|[0-9a-fA-F]{128})$`)

// ImageFromConfig reads config's image; nil when the block has none.
func ImageFromConfig(cfg map[string]any) (*Image, error) {
	m, ok := cfg["image"].(map[string]any)
	if !ok {
		return nil, nil
	}
	str := func(k string) string { s, _ := m[k].(string); return strings.TrimSpace(s) }
	im := &Image{URL: str("url"), Checksum: strings.ToLower(str("checksum")), ChecksumURL: str("checksumUrl")}
	if !httpURL(im.URL) {
		return nil, fmt.Errorf("image.url %q: want an http or https URL", im.URL)
	}
	switch {
	case im.Checksum != "":
		if !digestRE.MatchString(im.Checksum) {
			return nil, fmt.Errorf("image.checksum: want a SHA-256 or SHA-512 digest in hex")
		}
	case im.ChecksumURL != "":
		if !httpURL(im.ChecksumURL) {
			return nil, fmt.Errorf("image.checksumUrl %q: want an http or https URL", im.ChecksumURL)
		}
	default:
		return nil, fmt.Errorf("image: set checksum or checksumUrl, so the download can be checked")
	}
	return im, nil
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// digest is the image's expected digest, fetching the sums file when only that is given.
func (im Image) digest(ctx context.Context, client *http.Client) (string, error) {
	if im.Checksum != "" {
		return im.Checksum, nil
	}
	body, err := get(ctx, client, im.ChecksumURL)
	if err != nil {
		return "", err
	}
	defer body.Close()
	sums, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", im.ChecksumURL, err)
	}
	u, _ := url.Parse(im.URL)
	return sumsLookup(sums, path.Base(u.Path))
}

var (
	gnuSumRE = regexp.MustCompile(`^([0-9a-fA-F]{64}|[0-9a-fA-F]{128}) [ *]?(?:\./)?(.+)$`)
	bsdSumRE = regexp.MustCompile(`^SHA(?:256|512) \((.+)\) = ([0-9a-fA-F]{64}|[0-9a-fA-F]{128})$`)
)

// sumsLookup finds file's digest in a sums file, in GNU ("<hex>  name") or BSD ("SHA256 (name) = <hex>") form.
func sumsLookup(sums []byte, file string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := gnuSumRE.FindStringSubmatch(line); m != nil && m[2] == file {
			return strings.ToLower(m[1]), nil
		}
		if m := bsdSumRE.FindStringSubmatch(line); m != nil && m[1] == file {
			return strings.ToLower(m[2]), nil
		}
	}
	return "", fmt.Errorf("the checksum file does not list %s", file)
}

func newHash(digest string) hash.Hash {
	if len(digest) == 128 {
		return sha512.New()
	}
	return sha256.New()
}

// Fetch returns the path of the image in dir, downloading and checking it first unless a
// file with its digest is already there. progress, if set, hears bytes done and the total (-1 if unknown).
func Fetch(ctx context.Context, client *http.Client, im Image, dir string, progress func(done, total int64)) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	want, err := im.digest(ctx, client)
	if err != nil {
		return "", err
	}
	cached := filepath.Join(dir, want)
	if _, err := os.Stat(cached); err == nil {
		return cached, nil
	}
	body, err := get(ctx, client, im.URL)
	if err != nil {
		return "", err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	h := newHash(want)
	w := io.MultiWriter(tmp, h)
	if progress != nil {
		w = &counter{w: w, total: body.size, fn: progress}
	}
	_, err = io.Copy(w, body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download %s: %w", im.URL, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("download %s: checksum %s, want %s", im.URL, got, want)
	}
	if err := os.Rename(tmp.Name(), cached); err != nil {
		return "", err
	}
	return cached, nil
}

type sizedBody struct {
	io.ReadCloser
	size int64
}

func get(ctx context.Context, client *http.Client, u string) (*sizedBody, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", u, resp.Status)
	}
	return &sizedBody{ReadCloser: resp.Body, size: resp.ContentLength}, nil
}

type counter struct {
	w           io.Writer
	done, total int64
	fn          func(done, total int64)
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.done += int64(n)
	c.fn(c.done, c.total)
	return n, err
}
