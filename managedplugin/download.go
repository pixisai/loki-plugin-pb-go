package managedplugin

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	loki_api "github.com/pixisai/loki-api-go"
	"github.com/schollz/progressbar/v3"
)

const (
	DefaultDownloadDir = ".loki"
	RetryAttempts      = 5
	RetryWaitTime      = 1 * time.Second
)

func APIBaseURL() string {
	const (
		envAPIURL  = "LOKI_API_URL"
		apiBaseURL = "https://api.lokipixis.ai"
	)

	if v := os.Getenv(envAPIURL); v != "" {
		return v
	}
	return apiBaseURL
}

// getURLLocation return the URL of the plugin
// this does a few HEAD requests because we had a few breaking changes to where
// we store the plugins on GitHub
// TODO: we can improve this by just embedding all plugins and last version that exist in different places then
// the latest
func getURLLocation(ctx context.Context, org string, name string, version string, typ PluginType) (string, error) {
	urls := []string{
		// TODO: add this back when we move to the new plugin system
		// fmt.Sprintf("https://github.com/%s/loki-plugin-%s/releases/download/%s/loki-%s_%s_%s.zip", org, name, version, name, runtime.GOOS, runtime.GOARCH),
		fmt.Sprintf("https://github.com/%s/loki-source-%s/releases/download/%s/loki-source-%s_%s_%s.zip", org, name, version, name, runtime.GOOS, runtime.GOARCH),
	}
	if org == "pixis" {
		// TODO: add this back when we move to the new plugin system
		// urls = append(urls, fmt.Sprintf("https://github.com/pixisai/loki/releases/download/plugins-%s-%s/%s_%s_%s.zip", name, version, name, runtime.GOOS, runtime.GOARCH))
		urls = append(urls, fmt.Sprintf("https://github.com/pixis/loki/releases/download/plugins-source-%s-%s/%s_%s_%s.zip", name, version, name, runtime.GOOS, runtime.GOARCH))
	}
	if typ == PluginDestination {
		urls = []string{
			// TODO: add this back when we move to the new plugin system
			// fmt.Sprintf("https://github.com/%s/loki-plugin-%s/releases/download/%s/loki-%s_%s_%s.zip", org, name, version, name, runtime.GOOS, runtime.GOARCH),
			fmt.Sprintf("https://github.com/%s/loki-destination-%s/releases/download/%s/loki-destination-%s_%s_%s.zip", org, name, version, name, runtime.GOOS, runtime.GOARCH),
		}
		if org == "pixis" {
			// TODO: add this back when we move to the new plugin system
			// urls = append(urls, fmt.Sprintf("https://github.com/pixisai/loki/releases/download/plugins-%s-%s/%s_%s_%s.zip", name, version, name, runtime.GOOS, runtime.GOARCH))
			urls = append(urls, fmt.Sprintf("https://github.com/pixisai/loki/releases/download/plugins-destination-%s-%s/%s_%s_%s.zip", name, version, name, runtime.GOOS, runtime.GOARCH))
		}
	}

	for _, downloadURL := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, downloadURL, nil)
		if err != nil {
			return "", fmt.Errorf("failed create request %s: %w", downloadURL, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("failed to get url %s: %w", downloadURL, err)
		}
		// Check server response
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue
		} else if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			fmt.Printf("Failed downloading %s with status code %d. Retrying\n", downloadURL, resp.StatusCode)
			return "", errors.New("statusCode != 200")
		}
		resp.Body.Close()
		return downloadURL, nil
	}

	return "", fmt.Errorf("failed to find plugin %s/%s version %s", org, name, version)
}

type HubDownloadOptions struct {
	AuthToken     string
	TeamName      string
	LocalPath     string
	PluginTeam    string
	PluginKind    string
	PluginName    string
	PluginVersion string
}

func DownloadPluginFromHub(ctx context.Context, ops HubDownloadOptions) error {
	downloadDir := filepath.Dir(ops.LocalPath)
	if _, err := os.Stat(ops.LocalPath); err == nil {
		return nil
	}

	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return fmt.Errorf("failed to create plugin directory %s: %w", downloadDir, err)
	}

	pluginAsset, statusCode, err := downloadPluginAssetFromHub(ctx, ops)
	if err != nil {
		return fmt.Errorf("failed to get plugin url: %w", err)
	}
	switch statusCode {
	case http.StatusOK:
		// we allow this status code
	case http.StatusUnauthorized:
		return fmt.Errorf("unauthorized. Try logging in via `loki login`")
	case http.StatusNotFound:
		return fmt.Errorf("failed to download plugin %v %v/%v@%v: plugin version not found. If you're trying to use a private plugin you'll need to run `loki login` first", ops.PluginKind, ops.PluginTeam, ops.PluginName, ops.PluginVersion)
	case http.StatusTooManyRequests:
		return fmt.Errorf("too many download requests. Try logging in via `loki login` to increase rate limits")
	default:
		return fmt.Errorf("failed to download plugin %v %v/%v@%v: unexpected status code %v", ops.PluginKind, ops.PluginTeam, ops.PluginName, ops.PluginVersion, statusCode)
	}
	if pluginAsset == nil {
		return fmt.Errorf("failed to get plugin url for %v %v/%v@%v: missing json response", ops.PluginKind, ops.PluginTeam, ops.PluginName, ops.PluginVersion)
	}
	location := pluginAsset.Location
	if len(location) == 0 {
		return fmt.Errorf("failed to get plugin url: empty location from response")
	}
	pluginZipPath := ops.LocalPath + ".zip"
	writtenChecksum, err := downloadFile(ctx, pluginZipPath, location)
	if err != nil {
		return fmt.Errorf("failed to download plugin: %w", err)
	}

	if pluginAsset.Checksum == "" {
		fmt.Printf("Warning - checksum not verified: %s\n", writtenChecksum)
	} else if writtenChecksum != pluginAsset.Checksum {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", pluginAsset.Checksum, writtenChecksum)
	}

	archive, err := zip.OpenReader(pluginZipPath)
	if err != nil {
		return fmt.Errorf("failed to open plugin archive: %w", err)
	}
	defer archive.Close()

	fileInArchive, err := archive.Open(fmt.Sprintf("plugin-%s-%s-%s-%s", ops.PluginName, ops.PluginVersion, runtime.GOOS, runtime.GOARCH))
	if err != nil {
		return fmt.Errorf("failed to open plugin archive: %w", err)
	}

	out, err := os.OpenFile(ops.LocalPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0744)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", ops.LocalPath, err)
	}
	_, err = io.Copy(out, fileInArchive)
	if err != nil {
		return fmt.Errorf("failed to copy body to file: %w", err)
	}
	err = out.Close()
	if err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}
	return nil
}

func downloadPluginAssetFromHub(ctx context.Context, ops HubDownloadOptions) (*loki_api.PluginAsset, int, error) {
	c, err := loki_api.NewClientWithResponses(APIBaseURL(),
		loki_api.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			if ops.AuthToken != "" {
				req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", ops.AuthToken))
			}
			return nil
		}))
	if err != nil {
		return nil, -1, fmt.Errorf("failed to create Hub API client: %w", err)
	}

	target := fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
	aj := "application/json"

	switch {
	case ops.TeamName != "":
		resp, err := c.DownloadPluginAssetByTeamWithResponse(
			ctx,
			ops.TeamName,
			ops.PluginTeam,
			loki_api.PluginKind(ops.PluginKind),
			ops.PluginName,
			ops.PluginVersion,
			target,
			&loki_api.DownloadPluginAssetByTeamParams{Accept: &aj},
		)
		if err != nil {
			return nil, -1, fmt.Errorf("failed to get plugin url with team: %w", err)
		}
		return resp.JSON200, resp.StatusCode(), nil
	default:
		resp, err := c.DownloadPluginAssetWithResponse(
			ctx,
			ops.PluginTeam,
			loki_api.PluginKind(ops.PluginKind),
			ops.PluginName,
			ops.PluginVersion,
			target,
			&loki_api.DownloadPluginAssetParams{Accept: &aj},
		)
		if err != nil {
			return nil, -1, fmt.Errorf("failed to get plugin url: %w", err)
		}
		return resp.JSON200, resp.StatusCode(), nil
	}
}

func DownloadPluginFromGithub(ctx context.Context, localPath string, org string, name string, version string, typ PluginType) error {
	downloadDir := filepath.Dir(localPath)
	pluginZipPath := localPath + ".zip"

	if _, err := os.Stat(localPath); err == nil {
		return nil
	}

	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return fmt.Errorf("failed to create plugin directory %s: %w", downloadDir, err)
	}

	downloadURL, err := getURLLocation(ctx, org, name, version, typ)
	if err != nil {
		return fmt.Errorf("failed to get plugin url: %w", err)
	}
	if _, err := downloadFile(ctx, pluginZipPath, downloadURL); err != nil {
		return fmt.Errorf("failed to download plugin: %w", err)
	}

	archive, err := zip.OpenReader(pluginZipPath)
	if err != nil {
		return fmt.Errorf("failed to open plugin archive: %w", err)
	}
	defer archive.Close()

	var pathInArchive string
	switch {
	case strings.HasPrefix(downloadURL, "https://github.com/pixisai/loki/releases/download/plugins-plugin"):
		pathInArchive = fmt.Sprintf("plugins/plugin/%s", name)
	case strings.HasPrefix(downloadURL, "https://github.com/pixisai/loki/releases/download/plugins-source"):
		pathInArchive = fmt.Sprintf("plugins/source/%s", name)
	case strings.HasPrefix(downloadURL, "https://github.com/pixisai/loki/releases/download/plugins-destination"):
		pathInArchive = fmt.Sprintf("plugins/destination/%s", name)
	case strings.HasPrefix(downloadURL, fmt.Sprintf("https://github.com/%s/loki-plugin", org)):
		pathInArchive = fmt.Sprintf("loki-plugin-%s", name)
	case strings.HasPrefix(downloadURL, fmt.Sprintf("https://github.com/%s/loki-source", org)):
		pathInArchive = fmt.Sprintf("loki-source-%s", name)
	case strings.HasPrefix(downloadURL, fmt.Sprintf("https://github.com/%s/loki-destination", org)):
		pathInArchive = fmt.Sprintf("loki-destination-%s", name)
	default:
		return fmt.Errorf("unknown GitHub %s", downloadURL)
	}

	pathInArchive = WithBinarySuffix(pathInArchive)
	fileInArchive, err := archive.Open(pathInArchive)
	if err != nil {
		return fmt.Errorf("failed to open plugin archive plugins/source/%s: %w", name, err)
	}
	out, err := os.OpenFile(localPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0744)
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", localPath, err)
	}
	_, err = io.Copy(out, fileInArchive)
	if err != nil {
		return fmt.Errorf("failed to copy body to file: %w", err)
	}
	err = out.Close()
	if err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}
	return nil
}

func downloadFile(ctx context.Context, localPath string, downloadURL string) (string, error) {
	// Create the file
	out, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("failed to create file %s: %w", localPath, err)
	}
	defer out.Close()

	return downloadFileFromURL(ctx, out, downloadURL)
}

func downloadFileFromURL(ctx context.Context, out *os.File, downloadURL string) (string, error) {
	checksum := ""
	err := retry.Do(func() error {
		checksum = ""
		// Get the data
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
		if err != nil {
			return fmt.Errorf("failed create request %s: %w", downloadURL, err)
		}

		// Do http request
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to get url %s: %w", downloadURL, err)
		}
		defer resp.Body.Close()
		// Check server response
		if resp.StatusCode == http.StatusNotFound {
			return errors.New("not found")
		} else if resp.StatusCode != http.StatusOK {
			fmt.Printf("Failed downloading %s with status code %d. Retrying\n", downloadURL, resp.StatusCode)
			return errors.New("statusCode != 200")
		}

		urlForLog := downloadURL
		parsedURL, err := url.Parse(downloadURL)
		if err == nil {
			parsedURL.RawQuery = ""
			parsedURL.Fragment = ""
			urlForLog = parsedURL.String()
		}
		fmt.Printf("Downloading %s\n", urlForLog)
		bar := downloadProgressBar(resp.ContentLength, "Downloading")

		s := sha256.New()
		// Writer the body to file
		_, err = io.Copy(io.MultiWriter(out, bar, s), resp.Body)
		if err != nil {
			return fmt.Errorf("failed to copy body to file %s: %w", out.Name(), err)
		}
		checksum = fmt.Sprintf("%x", s.Sum(nil))
		return nil
	}, retry.RetryIf(func(err error) bool {
		return err.Error() == "statusCode != 200"
	}),
		retry.Attempts(RetryAttempts),
		retry.Delay(RetryWaitTime),
	)
	if err != nil {
		for _, e := range err.(retry.Error) {
			if e.Error() == "not found" {
				return "", e
			}
		}
		return "", fmt.Errorf("failed downloading URL %q. Error %w", downloadURL, err)
	}
	return checksum, nil
}

func downloadProgressBar(maxBytes int64, description ...string) *progressbar.ProgressBar {
	desc := ""
	if len(description) > 0 {
		desc = description[0]
	}
	return progressbar.NewOptions64(
		maxBytes,
		progressbar.OptionSetDescription(desc),
		progressbar.OptionSetWriter(os.Stdout),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(10),
		progressbar.OptionThrottle(65*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(os.Stdout, "\n")
		}),
		progressbar.OptionSpinnerType(14),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetRenderBlankState(true),
	)
}

func WithBinarySuffix(filePath string) string {
	if runtime.GOOS == "windows" {
		return filePath + ".exe"
	}
	return filePath
}
