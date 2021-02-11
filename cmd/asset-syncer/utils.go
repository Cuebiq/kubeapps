/*
Copyright (c) 2018 The Helm Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"
	"github.com/ghodss/yaml"
	"github.com/jinzhu/copier"
	"github.com/kubeapps/common/datastore"
	"github.com/kubeapps/kubeapps/pkg/chart/models"
	log "github.com/sirupsen/logrus"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	helmrepo "k8s.io/helm/pkg/repo"
)

const (
	defaultTimeoutSeconds = 10
	additionalCAFile      = "/usr/local/share/ca-certificates/ca.crt"
)

type importChartFilesJob struct {
	Name         string
	Repo         *models.Repo
	ChartVersion models.ChartVersion
}

type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

var netClient httpClient = &http.Client{}

func parseRepoURL(repoURL string) (*url.URL, error) {
	repoURL = strings.TrimSpace(repoURL)
	return url.ParseRequestURI(repoURL)
}

func init() {
	var err error
	netClient, err = initNetClient(additionalCAFile)
	if err != nil {
		log.Fatal(err)
	}
}

type assetManager interface {
	Delete(repo models.Repo) error
	Sync(repo models.Repo, charts []models.Chart) error
	RepoAlreadyProcessed(repo models.Repo, checksum string) bool
	UpdateLastCheck(repoNamespace, repoName, checksum string, now time.Time) error
	Init() error
	Close() error
	InvalidateCache() error
	updateIcon(repo models.Repo, data []byte, contentType, ID string) error
	filesExist(repo models.Repo, chartFilesID, digest string) bool
	insertFiles(chartID string, files models.ChartFiles) error
}

func newManager(config datastore.Config, kubeappsNamespace string) (assetManager, error) {
	return newPGManager(config, kubeappsNamespace)
}

func getSha256(src []byte) (string, error) {
	f := bytes.NewReader(src)
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Repo defines the methods to retrive information from the given repository
type Repo interface {
	Checksum() (string, error)
	Repo() *models.RepoInternal
	Charts() ([]models.Chart, error)
	FetchFiles(name string, cv models.ChartVersion) (map[string]string, error)
	FetchAllFilesFromDirectory(name string, cv models.ChartVersion, directoryName string) (map[string]string, error)

}

// HelmRepo implements the Repo interface for chartmuseum-like repositories
type HelmRepo struct {
	content []byte
	*models.RepoInternal
}

// Checksum returns the sha256 of the repo
func (r *HelmRepo) Checksum() (string, error) {
	return getSha256(r.content)
}

// Repo returns the repo information
func (r *HelmRepo) Repo() *models.RepoInternal {
	return r.RepoInternal
}

// Charts retrieve the list of charts exposed in the repo
func (r *HelmRepo) Charts() ([]models.Chart, error) {
	index, err := parseRepoIndex(r.content)
	if err != nil {
		return []models.Chart{}, err
	}

	repo := &models.Repo{
		Namespace: r.Namespace,
		Name:      r.Name,
		URL:       r.URL,
		Type:      r.Type,
	}
	charts := chartsFromIndex(index, repo)
	if len(charts) == 0 {
		return []models.Chart{}, fmt.Errorf("no charts in repository index")
	}

	return charts, nil
}

const (
	readme = "readme"
	values = "values"
	schema = "schema"
)


// FetchFiles retrieves the important files of a chart and version from the repo
func (r *HelmRepo) FetchAllFilesFromDirectory(name string, cv models.ChartVersion, directoryName string) (map[string]string, error) {
	chartTarballURL := chartTarballURL(r.RepoInternal, cv)
	req, err := http.NewRequest("GET", chartTarballURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	if len(r.AuthorizationHeader) > 0 {
		req.Header.Set("Authorization", r.AuthorizationHeader)
	}

	res, err := netClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	// We read the whole chart into memory, this should be okay since the chart
	// tarball needs to be small enough to fit into a GRPC call (Tiller
	// requirement)
	gzf, err := gzip.NewReader(res.Body)
	if err != nil {
		return nil, err
	}
	defer gzf.Close()

	tarf := tar.NewReader(gzf)

	// decode escaped characters
	// ie., "foo%2Fbar" should return "foo/bar"
	decodedName, err := url.PathUnescape(name)
	if err != nil {
		log.Errorf("Cannot decode %s", name)
		return nil, err
	}

	// get last part of the name
	// ie., "foo/bar" should return "bar"
	fixedName := path.Base(decodedName)
	directoryPath := fixedName +"/"+ directoryName

	filesInDirectory, err := extractDirectoryFilesFromTarball(directoryPath, tarf)
	if err != nil {
		return nil, err
	}

	return filesInDirectory, nil
}


// FetchFiles retrieves the important files of a chart and version from the repo
func (r *HelmRepo) FetchFiles(name string, cv models.ChartVersion) (map[string]string, error) {
	chartTarballURL := chartTarballURL(r.RepoInternal, cv)
	req, err := http.NewRequest("GET", chartTarballURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	if len(r.AuthorizationHeader) > 0 {
		req.Header.Set("Authorization", r.AuthorizationHeader)
	}

	res, err := netClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	// We read the whole chart into memory, this should be okay since the chart
	// tarball needs to be small enough to fit into a GRPC call (Tiller
	// requirement)
	gzf, err := gzip.NewReader(res.Body)
	if err != nil {
		return nil, err
	}
	defer gzf.Close()

	tarf := tar.NewReader(gzf)

	// decode escaped characters
	// ie., "foo%2Fbar" should return "foo/bar"
	decodedName, err := url.PathUnescape(name)
	if err != nil {
		log.Errorf("Cannot decode %s", name)
		return nil, err
	}

	// get last part of the name
	// ie., "foo/bar" should return "bar"
	fixedName := path.Base(decodedName)
	readmeFileName := fixedName + "/README.md"
	valuesFileName := fixedName + "/values.yaml"
	schemaFileName := fixedName + "/values.schema.json"
	filenames := map[string]string{
		values: valuesFileName,
		readme: readmeFileName,
		schema: schemaFileName,
	}

	files, err := extractFilesFromTarball(filenames, tarf)
	if err != nil {
		return nil, err
	}

	return map[string]string{
		values: files[values],
		readme: files[readme],
		schema: files[schema],
	}, nil
}

// TagList represents a list of tags as specified at
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#content-discovery
type TagList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// OCIRegistry implements the Repo interface for OCI repositories
type OCIRegistry struct {
	repositories []string
	*models.RepoInternal
	tags map[string]TagList
}

func doReq(url, authHeader string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", userAgent())
	if len(authHeader) > 0 {
		req.Header.Set("Authorization", authHeader)
	}

	res, err := netClient.Do(req)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return nil, err
	}

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request failed: %v", err)
	}

	return ioutil.ReadAll(res.Body)
}

// Checksum returns the sha256 of the repo by concatenating tags for
// all repositories within the registry and returning the sha256.
func (r *OCIRegistry) Checksum() (string, error) {
	content := []byte{}
	tags := map[string]TagList{}
	for _, appName := range r.repositories {
		url, err := parseRepoURL(r.RepoInternal.URL)
		if err != nil {
			return "", err
		}
		// Retrieve the list of tags to add it to the list
		// Caveat: Mutated image tags won't be detected as new
		url.Path = path.Join("v2", url.Path, appName, "tags", "list")
		data, err := doReq(url.String(), r.RepoInternal.AuthorizationHeader)
		if err != nil {
			return "", err
		}

		var appTags TagList
		err = json.Unmarshal(data, &appTags)
		if err != nil {
			return "", err
		}
		tags[appName] = appTags
		content = append(content, data...)
	}
	r.tags = tags

	return getSha256(content)
}

// Repo returns the repo information
func (r *OCIRegistry) Repo() *models.RepoInternal {
	return r.RepoInternal
}

type artifactFiles struct {
	Metadata string
	Readme   string
	Values   string
	Schema   string
}

func extractFilesFromBuffer(buf *bytes.Buffer) (*artifactFiles, error) {
	result := &artifactFiles{}
	gzf, err := gzip.NewReader(buf)
	if err != nil {
		return nil, err
	}
	tarReader := tar.NewReader(gzf)
	importantFiles := map[string]bool{
		"chart.yaml":         true,
		"readme.md":          true,
		"values.yaml":        true,
		"values.schema.json": true,
	}
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		compressedFileName := header.Name

		switch header.Typeflag {
		case tar.TypeDir:
			// Ignore directories
		case tar.TypeReg:
			filename := strings.ToLower(path.Base(compressedFileName))
			if importantFiles[filename] {
				// Read content
				data := make([]byte, header.Size)
				_, err := tarReader.Read(data)
				if err != nil && err != io.EOF {
					return nil, fmt.Errorf("failed to read %s. Got: %v", compressedFileName, err)
				}
				for err != io.EOF {
					_, err = tarReader.Read(data)
				}
				switch filename {
				case "chart.yaml":
					result.Metadata = string(data)
				case "readme.md":
					result.Readme = string(data)
				case "values.yaml":
					result.Values = string(data)
				case "values.schema.json":
					result.Schema = string(data)
				}
			}
		default:
			// Unkown type, ignore
		}
	}
	return result, nil
}

// Charts retrieve the list of charts exposed in the repo
func (r *OCIRegistry) Charts() ([]models.Chart, error) {
	// TBD
	return []models.Chart{}, nil
}

// FetchFiles retrieves the important files of a chart and version from the repo
func (r *OCIRegistry) FetchFiles(name string, cv models.ChartVersion) (map[string]string, error) {
	// TBD
	return map[string]string{
		values: "",
		readme: "",
		schema: "",
	}, nil
}

func (r *OCIRegistry) FetchAllFilesFromDirectory(name string, cv models.ChartVersion, directoryName string) (map[string]string, error){
    // TBD
    return map[string]string{}, nil
}


func getHelmRepo(namespace, name, repoURL, authorizationHeader string) (Repo, error) {
	url, err := parseRepoURL(repoURL)
	if err != nil {
		log.WithFields(log.Fields{"url": repoURL}).WithError(err).Error("failed to parse URL")
		return nil, err
	}

	repoBytes, err := fetchRepoIndex(url.String(), authorizationHeader)
	if err != nil {
		return nil, err
	}

	return &HelmRepo{content: repoBytes, RepoInternal: &models.RepoInternal{Namespace: namespace, Name: name, URL: url.String(), AuthorizationHeader: authorizationHeader}}, nil
}

func getOCIRepo(namespace, name, repoURL, authorizationHeader string, ociRepos []string) (Repo, error) {
	url, err := parseRepoURL(repoURL)
	if err != nil {
		log.WithFields(log.Fields{"url": repoURL}).WithError(err).Error("failed to parse URL")
		return nil, err
	}
	return &OCIRegistry{
		repositories: ociRepos,
		RepoInternal: &models.RepoInternal{Namespace: namespace, Name: name, URL: url.String(), AuthorizationHeader: authorizationHeader},
	}, nil
}

func fetchRepoIndex(url, authHeader string) ([]byte, error) {
	indexURL, err := parseRepoURL(url)
	if err != nil {
		log.WithFields(log.Fields{"url": url}).WithError(err).Error("failed to parse URL")
		return nil, err
	}
	indexURL.Path = path.Join(indexURL.Path, "index.yaml")
	return doReq(indexURL.String(), authHeader)
}

func parseRepoIndex(body []byte) (*helmrepo.IndexFile, error) {
	var index helmrepo.IndexFile
	err := yaml.Unmarshal(body, &index)
	if err != nil {
		return nil, err
	}
	index.SortEntries()
	return &index, nil
}

func chartsFromIndex(index *helmrepo.IndexFile, r *models.Repo) []models.Chart {
	var charts []models.Chart
	for _, entry := range index.Entries {
		if entry[0].GetDeprecated() {
			log.WithFields(log.Fields{"name": entry[0].GetName()}).Info("skipping deprecated chart")
			continue
		}
		charts = append(charts, newChart(entry, r))
	}
	sort.Slice(charts, func(i, j int) bool { return charts[i].ID < charts[j].ID })
	return charts
}

// Takes an entry from the index and constructs a database representation of the
// object.
func newChart(entry helmrepo.ChartVersions, r *models.Repo) models.Chart {
	var c models.Chart
	copier.Copy(&c, entry[0])
	copier.Copy(&c.ChartVersions, entry)
	c.Repo = r
	c.Name = url.PathEscape(c.Name) // escaped chart name eg. foo/bar becomes foo%2Fbar
	c.ID = fmt.Sprintf("%s/%s", r.Name, c.Name)
	c.Category = entry[0].Annotations["category"]
	return c
}

func extractDirectoryFilesFromTarball(directoryPath string, tarf *tar.Reader) (map[string]string, error) {
    ret := make(map[string]string)
    for {
        header, err := tarf.Next()
        if err == io.EOF {
            break
        }
        if err != nil {
            return ret, err
        }

        if strings.HasPrefix(header.Name, directoryPath) {
            var b bytes.Buffer
            io.Copy(&b, tarf)
            //TODO headear.name take only the files part
            ret[header.Name] = string(b.Bytes())

        }

     }
     return ret, nil
}

func extractFilesFromTarball(filenames map[string]string, tarf *tar.Reader) (map[string]string, error) {
	ret := make(map[string]string)
	for {
		header, err := tarf.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ret, err
		}

		for id, f := range filenames {
			if strings.EqualFold(header.Name, f) {
				var b bytes.Buffer
				io.Copy(&b, tarf)
				ret[id] = string(b.Bytes())
				break
			}
		}
	}
	return ret, nil
}

func chartTarballURL(r *models.RepoInternal, cv models.ChartVersion) string {
	source := cv.URLs[0]
	if _, err := parseRepoURL(source); err != nil {
		// If the chart URL is not absolute, join with repo URL. It's fine if the
		// URL we build here is invalid as we can catch this error when actually
		// making the request
		u, _ := url.Parse(r.URL)
		u.Path = path.Join(u.Path, source)
		return u.String()
	}
	return source
}

func initNetClient(additionalCA string) (*http.Client, error) {
	// Get the SystemCertPool, continue with an empty pool on error
	caCertPool, _ := x509.SystemCertPool()
	if caCertPool == nil {
		caCertPool = x509.NewCertPool()
	}

	// If additionalCA exists, load it
	if _, err := os.Stat(additionalCA); !os.IsNotExist(err) {
		certs, err := ioutil.ReadFile(additionalCA)
		if err != nil {
			return nil, fmt.Errorf("Failed to append %s to RootCAs: %v", additionalCA, err)
		}

		// Append our cert to the system pool
		if ok := caCertPool.AppendCertsFromPEM(certs); !ok {
			return nil, fmt.Errorf("Failed to append %s to RootCAs", additionalCA)
		}
	}

	// Return Transport for testing purposes
	return &http.Client{
		Timeout: time.Second * defaultTimeoutSeconds,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: caCertPool,
			},
			Proxy: http.ProxyFromEnvironment,
		},
	}, nil
}

type fileImporter struct {
	manager assetManager
}

func (f *fileImporter) fetchFiles(charts []models.Chart, repo Repo) {
	// Process 10 charts at a time
	numWorkers := 10
	iconJobs := make(chan models.Chart, numWorkers)
	chartFilesJobs := make(chan importChartFilesJob, numWorkers)
	var wg sync.WaitGroup

	log.Debugf("starting %d workers", numWorkers)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go f.importWorker(&wg, iconJobs, chartFilesJobs, repo)
	}

	// Enqueue jobs to process chart icons
	for _, c := range charts {
		iconJobs <- c
	}
	// Close the iconJobs channel to signal the worker pools to move on to the
	// chart files jobs
	close(iconJobs)

	// Iterate through the list of charts and enqueue the latest chart version to
	// be processed. Append the rest of the chart versions to a list to be
	// enqueued later
	var toEnqueue []importChartFilesJob
	for _, c := range charts {
		chartFilesJobs <- importChartFilesJob{c.Name, c.Repo, c.ChartVersions[0]}
		for _, cv := range c.ChartVersions[1:] {
			toEnqueue = append(toEnqueue, importChartFilesJob{c.Name, c.Repo, cv})
		}
	}

	// Enqueue all the remaining chart versions
	for _, cfj := range toEnqueue {
		chartFilesJobs <- cfj
	}
	// Close the chartFilesJobs channel to signal the worker pools that there are
	// no more jobs to process
	close(chartFilesJobs)

	// Wait for the worker pools to finish processing
	wg.Wait()
}

func (f *fileImporter) importWorker(wg *sync.WaitGroup, icons <-chan models.Chart, chartFiles <-chan importChartFilesJob, repo Repo) {
	defer wg.Done()
	for c := range icons {
		log.WithFields(log.Fields{"name": c.Name}).Debug("importing icon")
		if err := f.fetchAndImportIcon(c, repo.Repo()); err != nil {
			log.WithFields(log.Fields{"name": c.Name}).WithError(err).Error("failed to import icon")
		}
	}
	for j := range chartFiles {
		log.WithFields(log.Fields{"name": j.Name, "version": j.ChartVersion.Version}).Debug("importing readme and values")
		if err := f.fetchAndImportFilesWithCustomDirectory(j.Name, "CustomFiles", repo, j.ChartVersion); err != nil {
			log.WithFields(log.Fields{"name": j.Name, "version": j.ChartVersion.Version}).WithError(err).Error("failed to import files")
		}
	}
}

func (f *fileImporter) fetchAndImportIcon(c models.Chart, r *models.RepoInternal) error {
	if c.Icon == "" {
		log.WithFields(log.Fields{"name": c.Name}).Info("icon not found")
		return nil
	}

	req, err := http.NewRequest("GET", c.Icon, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent())
	if len(r.AuthorizationHeader) > 0 {
		req.Header.Set("Authorization", r.AuthorizationHeader)
	}

	res, err := netClient.Do(req)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return err
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%d %s", res.StatusCode, c.Icon)
	}

	b := []byte{}
	contentType := ""
	var img image.Image
	// if the icon is in any other format try to convert it to PNG
	if strings.Contains(res.Header.Get("Content-Type"), "image/svg") {
		// if the icon is an SVG, it requires special processing
		icon, err := oksvg.ReadIconStream(res.Body)
		if err != nil {
			log.WithFields(log.Fields{"name": c.Name}).WithError(err).Error("failed to decode icon")
			return err
		}
		w, h := int(icon.ViewBox.W), int(icon.ViewBox.H)
		icon.SetTarget(0, 0, float64(w), float64(h))
		rgba := image.NewNRGBA(image.Rect(0, 0, w, h))
		icon.Draw(rasterx.NewDasher(w, h, rasterx.NewScannerGV(w, h, rgba, rgba.Bounds())), 1)
		img = rgba
	} else {
		img, err = imaging.Decode(res.Body)
		if err != nil {
			log.WithFields(log.Fields{"name": c.Name}).WithError(err).Error("failed to decode icon")
			return err
		}
	}

	// TODO: make this configurable?
	resizedImg := imaging.Fit(img, 160, 160, imaging.Lanczos)
	var buf bytes.Buffer
	imaging.Encode(&buf, resizedImg, imaging.PNG)
	b = buf.Bytes()
	contentType = "image/png"

	return f.manager.updateIcon(models.Repo{Namespace: r.Namespace, Name: r.Name}, b, contentType, c.ID)
}

func (f *fileImporter) fetchAndImportFiles(name string, repo Repo, cv models.ChartVersion) error {
	r := repo.Repo()
	chartID := fmt.Sprintf("%s/%s", r.Name, name)
	chartFilesID := fmt.Sprintf("%s-%s", chartID, cv.Version)

	// Check if we already have indexed files for this chart version and digest
	if f.manager.filesExist(models.Repo{Namespace: r.Namespace, Name: r.Name}, chartFilesID, cv.Digest) {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Debug("skipping existing files")
		return nil
	}
	log.WithFields(log.Fields{"name": name, "version": cv.Version}).Debug("fetching files")

	files, err := repo.FetchFiles(name, cv)
	if err != nil {
		return err
	}

	chartFiles := models.ChartFiles{ID: chartFilesID, Repo: &models.Repo{Name: r.Name, Namespace: r.Namespace, URL: r.URL}, Digest: cv.Digest}
	if v, ok := files[readme]; ok {
		chartFiles.Readme = v
	} else {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Info("README.md not found")
	}
	if v, ok := files[values]; ok {
		chartFiles.Values = v
	} else {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Info("values.yaml not found")
	}
	if v, ok := files[schema]; ok {
		chartFiles.Schema = v
	} else {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Info("values.schema.json not found")
	}

	// inserts the chart files if not already indexed, or updates the existing
	// entry if digest has changed
	return f.manager.insertFiles(chartID, chartFiles)
}


func (f *fileImporter) fetchAndImportFilesWithCustomDirectory(name string, customDirectoryName string , repo Repo, cv models.ChartVersion) error {
	r := repo.Repo()
	chartID := fmt.Sprintf("%s/%s", r.Name, name)
	chartFilesID := fmt.Sprintf("%s-%s", chartID, cv.Version)

	// Check if we already have indexed files for this chart version and digest
	if f.manager.filesExist(models.Repo{Namespace: r.Namespace, Name: r.Name}, chartFilesID, cv.Digest) {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Debug("skipping existing files")
		return nil
	}
	log.WithFields(log.Fields{"name": name, "version": cv.Version}).Debug("fetching files")

	files, err := repo.FetchFiles(name, cv)
	if err != nil {
		return err
	}

	customFiles, err := repo.FetchAllFilesFromDirectory(name, cv, customDirectoryName )
    if err != nil {
    	return err
    }


	chartFiles := models.ChartFiles{ID: chartFilesID, Repo: &models.Repo{Name: r.Name, Namespace: r.Namespace, URL: r.URL}, Digest: cv.Digest, CustomFiles: customFiles}
	if v, ok := files[readme]; ok {
		chartFiles.Readme = v
	} else {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Info("README.md not found")
	}
	if v, ok := files[values]; ok {
		chartFiles.Values = v
	} else {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Info("values.yaml not found")
	}
	if v, ok := files[schema]; ok {
		chartFiles.Schema = v
	} else {
		log.WithFields(log.Fields{"name": name, "version": cv.Version}).Info("values.schema.json not found")
	}

	// inserts the chart files if not already indexed, or updates the existing
	// entry if digest has changed
	return f.manager.insertFiles(chartID, chartFiles)
}
