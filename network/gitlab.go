package network

import (
	"fmt"
	"github.com/Sirupsen/logrus"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/common"
	"gitlab.com/gitlab-org/gitlab-ci-multi-runner/helpers"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

const clientError = -100

type GitLabClient struct {
	clients map[string]*client
}

func (n *GitLabClient) getClient(runner common.RunnerCredentials) (c *client, err error) {
	if n.clients == nil {
		n.clients = make(map[string]*client)
	}
	key := fmt.Sprintf("%s_%s", runner.URL, runner.TLSCAFile)
	c = n.clients[key]
	if c == nil {
		c, err = newClient(runner)
		if err != nil {
			return
		}
		n.clients[key] = c
	}
	return
}

func (n *GitLabClient) getRunnerVersion(config common.RunnerConfig) common.VersionInfo {
	info := common.VersionInfo{
		Name:         common.NAME,
		Version:      common.VERSION,
		Revision:     common.REVISION,
		Platform:     runtime.GOOS,
		Architecture: runtime.GOARCH,
		Executor:     config.Executor,
	}

	if executor := common.GetExecutor(config.Executor); executor != nil {
		executor.GetFeatures(&info.Features)
	}

	if shell := common.GetShell(config.Shell); shell != nil {
		shell.GetFeatures(&info.Features)
	}

	return info
}

func (n *GitLabClient) doRaw(runner common.RunnerCredentials, method, uri string, request io.Reader, requestType string, headers http.Header) (res *http.Response, err error) {
	c, err := n.getClient(runner)
	if err != nil {
		return nil, err
	}

	return c.do(uri, method, request, requestType, headers)
}

func (n *GitLabClient) doJSON(runner common.RunnerCredentials, method, uri string, statusCode int, request interface{}, response interface{}) (int, string, string) {
	c, err := n.getClient(runner)
	if err != nil {
		return clientError, err.Error(), ""
	}

	return c.doJSON(uri, method, statusCode, request, response)
}

func (n *GitLabClient) GetBuild(config common.RunnerConfig) (*common.GetBuildResponse, bool) {
	request := common.GetBuildRequest{
		Info:  n.getRunnerVersion(config),
		Token: config.Token,
	}

	var response common.GetBuildResponse
	result, statusText, certificates := n.doJSON(config.RunnerCredentials, "POST", "builds/register.json", 201, &request, &response)

	switch result {
	case 201:
		config.Log().Println("Checking for builds...", "received")
		response.TLSCAChain = certificates
		return &response, true
	case 403:
		config.Log().Errorln("Checking for builds...", "forbidden")
		return nil, false
	case 204, 404:
		config.Log().Debugln("Checking for builds...", "nothing")
		return nil, true
	case clientError:
		config.Log().WithField("status", statusText).Errorln("Checking for builds...", "error")
		return nil, false
	default:
		config.Log().WithField("status", statusText).Warningln("Checking for builds...", "failed")
		return nil, true
	}
}

func (n *GitLabClient) RegisterRunner(runner common.RunnerCredentials, description, tags string) *common.RegisterRunnerResponse {
	// TODO: pass executor
	request := common.RegisterRunnerRequest{
		Info:        n.getRunnerVersion(common.RunnerConfig{}),
		Token:       runner.Token,
		Description: description,
		Tags:        tags,
	}

	var response common.RegisterRunnerResponse
	result, statusText, _ := n.doJSON(runner, "POST", "runners/register.json", 201, &request, &response)

	switch result {
	case 201:
		runner.Log().Println("Registering runner...", "succeeded")
		return &response
	case 403:
		runner.Log().Errorln("Registering runner...", "forbidden (check registration token)")
		return nil
	case clientError:
		runner.Log().WithField("status", statusText).Errorln("Registering runner...", "error")
		return nil
	default:
		runner.Log().WithField("status", statusText).Errorln("Registering runner...", "failed")
		return nil
	}
}

func (n *GitLabClient) DeleteRunner(runner common.RunnerCredentials) bool {
	request := common.DeleteRunnerRequest{
		Token: runner.Token,
	}

	result, statusText, _ := n.doJSON(runner, "DELETE", "runners/delete", 200, &request, nil)

	switch result {
	case 200:
		runner.Log().Println("Deleting runner...", "succeeded")
		return true
	case 403:
		runner.Log().Errorln("Deleting runner...", "forbidden")
		return false
	case clientError:
		runner.Log().WithField("status", statusText).Errorln("Deleting runner...", "error")
		return false
	default:
		runner.Log().WithField("status", statusText).Errorln("Deleting runner...", "failed")
		return false
	}
}

func (n *GitLabClient) VerifyRunner(runner common.RunnerCredentials) bool {
	request := common.VerifyRunnerRequest{
		Token: runner.Token,
	}

	// HACK: we use non-existing build id to check if receive forbidden or not found
	result, statusText, _ := n.doJSON(runner, "PUT", fmt.Sprintf("builds/%d", -1), 200, &request, nil)

	switch result {
	case 404:
		// this is expected due to fact that we ask for non-existing job
		runner.Log().Println("Verifying runner...", "is alive")
		return true
	case 403:
		runner.Log().Errorln("Verifying runner...", "is removed")
		return false
	case clientError:
		runner.Log().WithField("status", statusText).Errorln("Verifying runner...", "error")
		return false
	default:
		runner.Log().WithField("status", statusText).Errorln("Verifying runner...", "failed")
		return true
	}
}

func (n *GitLabClient) UpdateBuild(config common.RunnerConfig, id int, state common.BuildState, trace string) common.UpdateState {
	request := common.UpdateBuildRequest{
		Info:  n.getRunnerVersion(config),
		Token: config.Token,
		State: state,
		Trace: trace,
	}

	result, statusText, _ := n.doJSON(config.RunnerCredentials, "PUT", fmt.Sprintf("builds/%d.json", id), 200, &request, nil)
	switch result {
	case 200:
		config.Log().Println(id, "Submitting build to coordinator...", "ok")
		return common.UpdateSucceeded
	case 404:
		config.Log().Warningln(id, "Submitting build to coordinator...", "aborted")
		return common.UpdateAbort
	case 403:
		config.Log().Errorln(id, "Submitting build to coordinator...", "forbidden")
		return common.UpdateAbort
	case clientError:
		config.Log().WithField("status", statusText).Errorln(id, "Submitting build to coordinator...", "error")
		return common.UpdateAbort
	default:
		config.Log().WithField("status", statusText).Warningln(id, "Submitting build to coordinator...", "failed")
		return common.UpdateFailed
	}
}

func (n *GitLabClient) createArtifactsForm(mpw *multipart.Writer, reader io.Reader, baseName string) error {
	wr, err := mpw.CreateFormFile("file", baseName)
	if err != nil {
		return err
	}

	_, err = io.Copy(wr, reader)
	if err != nil {
		return err
	}
	return nil
}

func (n *GitLabClient) UploadRawArtifacts(config common.BuildCredentials, reader io.Reader, baseName string) common.UploadState {
	pr, pw := io.Pipe()
	defer pr.Close()

	mpw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer mpw.Close()
		err := n.createArtifactsForm(mpw, reader, baseName)
		if err != nil {
			pw.CloseWithError(err)
		}
	}()

	// TODO: Create proper interface for `doRaw` that can use other types than RunnerCredentials
	mappedConfig := common.RunnerCredentials{
		URL:       config.URL,
		Token:     config.Token,
		TLSCAFile: config.TLSCAFile,
	}

	headers := make(http.Header)
	headers.Set("BUILD-TOKEN", config.Token)
	res, err := n.doRaw(mappedConfig, "POST", fmt.Sprintf("builds/%d/artifacts", config.ID), pr, mpw.FormDataContentType(), headers)

	log := logrus.WithFields(logrus.Fields{
		"id":    config.ID,
		"token": helpers.ShortenToken(config.Token),
	})

	if err != nil {
		log.Errorln("Uploading artifacts to coordinator...", "error", err.Error())
		return common.UploadFailed
	}
	defer res.Body.Close()
	defer io.Copy(ioutil.Discard, res.Body)

	switch res.StatusCode {
	case 201:
		log.Println("Uploading artifacts to coordinator...", "ok")
		return common.UploadSucceeded
	case 403:
		log.Errorln("Uploading artifacts to coordinator...", "forbidden")
		return common.UploadForbidden
	case 413:
		log.Errorln("Uploading artifacts to coordinator...", "too large archive")
		return common.UploadTooLarge
	default:
		log.Warningln("Uploading artifacts to coordinator...", "failed", res.Status)
		return common.UploadFailed
	}
}

func (n *GitLabClient) UploadArtifacts(config common.BuildCredentials, artifactsFile string) common.UploadState {
	log := logrus.WithFields(logrus.Fields{
		"id":    config.ID,
		"token": helpers.ShortenToken(config.Token),
	})

	file, err := os.Open(artifactsFile)
	if err != nil {
		log.Errorln("Uploading artifacts to coordinator...", "error", err.Error())
		return common.UploadFailed
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		log.Errorln("Uploading artifacts to coordinator...", "error", err.Error())
		return common.UploadFailed
	}
	if fi.IsDir() {
		log.Errorln("Uploading artifacts to coordinator...", "error", "cannot upload directories")
		return common.UploadFailed
	}

	baseName := filepath.Base(artifactsFile)
	return n.UploadRawArtifacts(config, file, baseName)
}

func (n *GitLabClient) DownloadArtifacts(config common.BuildCredentials, artifactsFile string) common.DownloadState {
	// TODO: Create proper interface for `doRaw` that can use other types than RunnerCredentials
	mappedConfig := common.RunnerCredentials{
		URL:       config.URL,
		Token:     config.Token,
		TLSCAFile: config.TLSCAFile,
	}

	headers := make(http.Header)
	headers.Set("BUILD-TOKEN", config.Token)
	res, err := n.doRaw(mappedConfig, "GET", fmt.Sprintf("builds/%d/artifacts", config.ID), nil, "", headers)

	log := logrus.WithFields(logrus.Fields{
		"id":    config.ID,
		"token": helpers.ShortenToken(config.Token),
	})

	if err != nil {
		log.Errorln("Uploading artifacts from coordinator...", "error", err.Error())
		return common.DownloadFailed
	}
	defer res.Body.Close()
	defer io.Copy(ioutil.Discard, res.Body)

	switch res.StatusCode {
	case 200:
		file, err := os.Create(artifactsFile)
		if err == nil {
			defer file.Close()
			_, err = io.Copy(file, res.Body)
		}
		if err != nil {
			file.Close()
			os.Remove(file.Name())
			log.Errorln("Uploading artifacts from coordinator...", "error", err.Error())
			return common.DownloadFailed
		}
		log.Println("Downloading artifacts from coordinator...", "ok")
		return common.DownloadSucceeded
	case 403:
		log.Errorln("Downloading artifacts from coordinator...", "forbidden")
		return common.DownloadForbidden
	case 404:
		log.Errorln("Downloading artifacts from coordinator...", "not found")
		return common.DownloadNotFound
	default:
		log.Warningln("Downloading artifacts from coordinator...", "failed", res.Status)
		return common.DownloadFailed
	}
}

func (n *GitLabClient) ProcessBuild(config common.RunnerConfig, id int) common.BuildTrace {
	trace := newBuildTrace(n, config, id)
	trace.start()
	return trace
}
