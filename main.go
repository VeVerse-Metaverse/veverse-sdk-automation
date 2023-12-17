package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/Masterminds/semver/v3"
	"github.com/gabriel-vasile/mimetype"
	"github.com/gofrs/uuid"
	"github.com/mholt/archiver/v4"
	"github.com/sirupsen/logrus"
	"gopkg.in/ini.v1"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const taskUploadPackageSource = "uploadPackageSource"
const taskUnzipPackageSource = "unzipPackageSource"
const taskUpdateSDK = "updateSDK"
const minChunkSize = 1 * 1024 * 1024

var (
	fVerbose   *bool   // Verbose output
	fLog       *bool   // Create debug log file
	fApiUrl    *string // APIv2 base url
	fToken     *string // APIv2 JWT
	fTask      *string // Task switch
	fProject   *string // Project name
	fPlugin    *string // Plugin name
	fEntityId  *string // Entity id
	fAppId     *string // App id
	fChunkSize *int64  // Chunk size
	apiUrl     string
	token      string
	task       string
	plugin     string
	project    string
	entityId   uuid.UUID
	appId      uuid.UUID
	chunkSize  int64
)

func errorExit() {
	flag.Usage()
	os.Exit(-1)
}

type Identifier struct {
	Id *uuid.UUID `json:"id,omitempty"`
}

type EntityTrait struct {
	Identifier
	EntityId *uuid.UUID `json:"entityId,omitempty"`
}

type Timestamps struct {
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

type FileMetadata struct {
	EntityTrait

	Type         string     `json:"type"`
	Url          string     `json:"url"`
	Mime         *string    `json:"mime,omitempty"`
	Size         *int64     `json:"size,omitempty"`
	Version      int        `json:"version,omitempty"`        // version of the file if versioned
	Deployment   string     `json:"deploymentType,omitempty"` // server or client if applicable
	Platform     string     `json:"platform,omitempty"`       // platform if applicable
	UploadedBy   *uuid.UUID `json:"uploadedBy,omitempty"`     // user that uploaded the file
	Width        *int       `json:"width,omitempty"`
	Height       *int       `json:"height,omitempty"`
	CreatedAt    time.Time  `json:"createdAt,omitempty"`
	UpdatedAt    *time.Time `json:"updatedAt,omitempty"`
	Index        int        `json:"variation,omitempty"`    // variant of the file if applicable (e.g. PDF pages)
	OriginalPath string     `json:"originalPath,omitempty"` // original relative path to maintain directory structure (e.g. for releases)

	Timestamps
}

type EntityMetadata struct {
	Identifier
	EntityType *string `json:"entityType,omitempty"`
	Public     *bool   `json:"public,omitempty"`

	Timestamps

	Files []FileMetadata `json:"files,omitempty"`
}

type ReleaseMetadata struct {
	EntityMetadata

	AppId          *uuid.UUID `json:"appId,omitempty"`
	AppName        string     `json:"appName,omitempty"`
	Version        string     `json:"version,omitempty"`
	Name           *string    `json:"name,omitempty"`
	Description    *string    `json:"description,omitempty"`
	AppDescription *string    `json:"appDescription"`
}

type ReleaseMetadataContainer struct {
	ReleaseMetadata `json:"data"`
	Status          string `json:"status,omitempty"`
	Message         string `json:"message,omitempty"`
}

func isProjectDir(projectName string, dir string) bool {
	items, err := os.ReadDir(dir)
	if err != nil {
		log.Fatal(err)
	}

	// Look for the uproject file in the current directory
	for _, item := range items {
		if projectName != "" {
			if strings.ToLower(item.Name()) == strings.ToLower(projectName+".uproject") {
				return true
			}
		} else {
			if strings.ToLower(filepath.Ext(item.Name())) == ".uproject" {
				return true
			}
		}
	}

	return false
}

func getProjectDir(projectName string) (string, error) {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return "", fmt.Errorf("failed to get cwd")
	}

	if isProjectDir(projectName, wd) {
		return wd, nil
	} else {
		volumeName := filepath.VolumeName(wd)
		rootDir := filepath.Join(volumeName, "/")
		cwd := wd
		for {
			cwd = filepath.Dir(cwd)
			if cwd == rootDir || cwd == "/" {
				return "", fmt.Errorf("failed to find the project dir")
			} else if isProjectDir(projectName, cwd) {
				return cwd, nil
			}
		}
	}
}

func getPluginDir(projectName string, pluginName string) (string, error) {
	projectDir, err := getProjectDir(projectName)
	if err != nil {
		return "", fmt.Errorf("failed to find the plugin directory: %v", err)
	}

	pluginDir := filepath.Join(projectDir, "Plugins", pluginName)

	return pluginDir, nil
}

func getPluginTempDir(projectName string, pluginName string) (string, error) {
	projectDir, err := getProjectDir(projectName)
	if err != nil {
		return "", fmt.Errorf("failed to find the plugin directory: %v", err)
	}

	pluginDir := filepath.Join(projectDir, "Plugins", pluginName, "Temp", pluginName)

	return pluginDir, nil
}

func getProjectVersion(projectName string) (version *semver.Version, err error) {
	var (
		projectDir string
	)

	// Get the project directory
	if projectDir, err = getProjectDir(projectName); err != nil {
		return nil, fmt.Errorf("failed to get project version: %v", err)
	}

	// Find and load the ini file
	var cfg *ini.File
	cfg, err = ini.Load(filepath.Join(projectDir, "Config", "DefaultGame.ini"))
	if err != nil {
		return nil, fmt.Errorf("failed to load ini: %v", err)
	}

	// Get the project version key
	versionKey := cfg.Section("/Script/EngineSettings.GeneralProjectSettings").Key("ProjectVersion").String()
	version, err = semver.NewVersion(versionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse version: %v", err)
	}

	return version, nil
}

// fetchUnclaimedJob Tries to fetch the unclaimed job supported by the runner, validates and returns it
func getLatestVersion() (version *semver.Version, err error) {
	// Prepare an HTTP request
	reqUrl := fmt.Sprintf("%s/apps/%s/releases/latest?platform=%s", apiUrl, appId)
	req, err := http.NewRequest("GET", reqUrl, nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	// Send HTTP request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %s", err)
	}

	// Process response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %s", err)
	}

	// Validate response
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to fetch an unclaimed job, status code: %d, content: %s", resp.StatusCode, string(body))
	}

	// Parse the HTTP request json content
	var container ReleaseMetadataContainer
	err = json.Unmarshal(body, &container)
	if err != nil {
		return nil, fmt.Errorf("failed to parse job json: %s", err.Error())
	}

	version, err = semver.NewVersion(container.ReleaseMetadata.Version)
	if err != nil {
		return nil, fmt.Errorf("failed to create semver: %v", err)
	}

	return version, nil
}

// uploadFile uploads the job results to the API for storage
func uploadEntityFile(entityId uuid.UUID, fileType string, fileMime string, path string, originalPath string, params map[string]string) error {
	const chunkSize = 100 * 1024 * 1024 // 100MiB

	if entityId.IsNil() {
		return fmt.Errorf("invalid job package id")
	}

	// Warning! For the package upload we don't set index and original-path to prevent duplicates, if these fields provided, we will get an error on DB index in future re-uploads of the package
	reqUrl := fmt.Sprintf("%s/entities/%s/files/upload?type=%s&mime=%s&original-path=%s", apiUrl, entityId.String(), fileType, fileMime, originalPath)

	// Open file
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %v", err)
	}

	// Get file info
	fi, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %v", err)
	}

	// Defer file close
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			logrus.Errorf("failed to close the uploading package file")
		}
	}(file)

	// Temporary buffer to get multipart form fields (header) and the boundary
	multipartFormBuffer := &bytes.Buffer{}

	// Add multipart form data parameters if any supplied
	multipartFormWriter := multipart.NewWriter(multipartFormBuffer)
	for key, value := range params {
		err = multipartFormWriter.WriteField(key, value)
	}

	// Add a file to the multipart form writer, the field name should be "file" as the API expects it
	_, err = multipartFormWriter.CreateFormFile("file", fi.Name())
	if err != nil {
		return fmt.Errorf("failed to create a multipart form file: %v", err)
	}

	// Get multipart form content type including boundary
	multipartFormDataContentType := multipartFormWriter.FormDataContentType()

	multipartFormOpeningHeaderSize := multipartFormBuffer.Len()

	// Save the opening multipart form header from the buffer
	multipartFormOpeningHeader := make([]byte, multipartFormOpeningHeaderSize)
	_, err = multipartFormBuffer.Read(multipartFormOpeningHeader)
	if err != nil {
		return fmt.Errorf("failed to read the multipart form buffer: %v", err)
	}

	// Write the multipart form closing boundary to the buffer
	err = multipartFormWriter.Close()
	if err != nil {
		return fmt.Errorf("failed to close the multipart form message")
	}

	multipartFormClosingBoundarySize := multipartFormBuffer.Len()

	// Save the closing multipart form boundary to the buffer
	multipartFormClosingBoundary := make([]byte, multipartFormClosingBoundarySize)
	_, err = multipartFormBuffer.Read(multipartFormClosingBoundary)
	if err != nil {
		return fmt.Errorf("failed to read boundary from the multipart form buffer")
	}

	// Calculate the total content size including opening header size, uploaded file size and closing boundary length
	multipartDataTotalSize := int64(multipartFormOpeningHeaderSize) + fi.Size() + int64(multipartFormClosingBoundarySize)

	// Use a pipe to write request data
	pipeReader, pipeWriter := io.Pipe()
	defer func(rd *io.PipeReader) {
		err := rd.Close()
		if err != nil {
			logrus.Errorf("failed to close a pipe reader")
		}
	}(pipeReader)

	var totalSent = 0

	go func() {
		defer func(pipeWriter *io.PipeWriter) {
			err := pipeWriter.Close()
			if err != nil {
				logrus.Errorf("failed to close a pipe writer: %v", err)
			}
		}(pipeWriter)

		// Write the multipart form opening header
		_, err = pipeWriter.Write(multipartFormOpeningHeader)
		if err != nil {
			logrus.Errorf("failed to write the opening header to the multipart form: %v", err)
		}

		// Write the file bytes to the temporary buffer
		buffer := make([]byte, chunkSize)
		for {
			n, err := file.Read(buffer)
			if err != nil {
				if err != io.EOF {
					logrus.Errorf("failed to read from the file pipe reader: %v", err)
				}
				break
			}

			logrus.Debugf("sending bytes '%d' to '%d'", totalSent, totalSent+n)
			totalSent += n

			_, err = pipeWriter.Write(buffer[:n])
			if err != nil {
				logrus.Errorf("failed to write file bytes to the multipart form: %v", err)
			}
		}

		// Write the closing boundary to the multipart form
		_, err = pipeWriter.Write(multipartFormClosingBoundary)
		if err != nil {
			logrus.Errorf("failed to write the closing boundary to the multipart form: %v", err)
		}
	}()

	// Create an HTTP request with the pipe reader
	req, err := http.NewRequest("PUT", reqUrl, pipeReader)
	req.Header.Set("Content-Type", multipartFormDataContentType)
	req.ContentLength = multipartDataTotalSize
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	// Process the HTTP request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %v", err)
	}

	defer func(body io.ReadCloser) {
		err := body.Close()
		if err != nil {
			logrus.Errorf("failed to close resp body: %v", err)
		}
	}(resp.Body)

	if resp.StatusCode >= 400 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read the response body: %v", err)
		}
		return fmt.Errorf("failed to upload a file, status code: %d, content: %s", resp.StatusCode, string(body))
	}

	return nil
}

type EntityUploadUrlPayload struct {
	Data FileMetadata `json:"data,omitempty"`
}

func getEntityFileUploadUrl(entityId uuid.UUID, fileType string, mime string, size int64, originalPath string) (FileMetadata, error) {
	reqUrl := fmt.Sprintf("%s/files/upload?entityId=%s&type=%s&mime=%s&size=%d&original-path=%s", apiUrl, entityId.String(), fileType, mime, size, originalPath)

	req, err := http.NewRequest("GET", reqUrl, nil)
	if err != nil {
		return FileMetadata{}, fmt.Errorf("failed to instantiate request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	// Process the HTTP request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return FileMetadata{}, fmt.Errorf("failed to send request: %v", err)
	}

	defer func(body io.ReadCloser) {
		err := body.Close()
		if err != nil {
			logrus.Errorf("failed to close resp body: %v", err)
		}
	}(resp.Body)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return FileMetadata{}, fmt.Errorf("failed to read the response body: %v", err)
	}

	if resp.StatusCode >= 400 {
		return FileMetadata{}, fmt.Errorf("failed to upload a file, status code: %d, content: %s", resp.StatusCode, string(body))
	}

	var container EntityUploadUrlPayload
	err = json.Unmarshal(body, &container)
	if err != nil {
		return FileMetadata{}, fmt.Errorf("failed to parse upload URL json: %s", err.Error())
	}

	return container.Data, nil
}

func createPackageJobs(entityId uuid.UUID) error {
	reqUrl := fmt.Sprintf("%s/jobs/package", apiUrl)

	m := map[string]string{"entityId": entityId.String()}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("failed to serialize entity id: %v", err)
	}

	req, err := http.NewRequest("POST", reqUrl, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))

	// Process the HTTP request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %v", err)
	}

	defer func(body io.ReadCloser) {
		err := body.Close()
		if err != nil {
			logrus.Errorf("failed to close resp body: %v", err)
		}
	}(resp.Body)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read the response body: %v", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("failed to upload a file, status code: %d, content: %s", resp.StatusCode, string(body))
	}

	return nil
}

func logUploadStatus(current int64, total int64) {
	logrus.Infof("u%d:%d|%.3f", current, total, float64(current)/float64(total))
}

// uploadFile uploads the job results to the API for storage
func uploadEntityFileToS3(presignedUrl string, entityId uuid.UUID, path string) error {
	if entityId.IsNil() {
		return fmt.Errorf("invalid job package id")
	}

	// Open file
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %v", err)
	}

	// Get file info
	fi, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat file: %v", err)
	}

	fileTotalSize := fi.Size()

	//region Determine MIME type

	// Seek back to the start of the file
	_, err = file.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("failed to rewind file before mime detection: %v", err)
	}

	// Try to detect MIME
	pMIME, _ := mimetype.DetectReader(file)
	fileContentType := pMIME.String()

	// Seek back to the start of the file
	_, err = file.Seek(0, io.SeekStart)
	if err != nil {
		logrus.Errorf("failed to rewind file after mime detection: %v", err)
	}

	//endregion

	// Defer file close
	defer func(file *os.File) {
		err := file.Close()
		if err != nil {
			logrus.Errorf("failed to close the uploading package file: %v", err)
		}
	}(file)

	// Use a pipe to write request data
	pipeReader, pipeWriter := io.Pipe()
	defer func(rd *io.PipeReader) {
		err := rd.Close()
		if err != nil {
			logrus.Errorf("failed to close a pipe reader")
		}
	}(pipeReader)

	var totalSent int64 = 0

	go func() {
		defer func(pipeWriter *io.PipeWriter) {
			err := pipeWriter.Close()
			if err != nil {
				logrus.Errorf("failed to close a pipe writer: %v", err)
			}
		}(pipeWriter)

		// Write the file bytes to the temporary buffer
		buffer := make([]byte, chunkSize)
		for {
			n, err := file.Read(buffer)
			if err != nil {
				if err != io.EOF {
					logrus.Errorf("failed to read from the file pipe reader: %v", err)
				}
				break
			}

			_, err = pipeWriter.Write(buffer[:n])
			if err != nil {
				logrus.Errorf("failed to write file bytes to the multipart form: %v", err)
			}

			totalSent += int64(n)
			logUploadStatus(totalSent, fileTotalSize)
		}
	}()

	logrus.Debugf("uploading to: %s", presignedUrl)

	// Create an HTTP request with the pipe reader
	req, err := http.NewRequest("PUT", presignedUrl, pipeReader)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", fileContentType)
	req.ContentLength = fileTotalSize
	req.Header.Set("Accept", "application/json")

	// Process the HTTP request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %v", err)
	}

	defer func(body io.ReadCloser) {
		err := body.Close()
		if err != nil {
			logrus.Errorf("failed to close resp body: %v", err)
		}
	}(resp.Body)

	if resp.StatusCode >= 400 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read the response body: %v", err)
		}
		return fmt.Errorf("failed to upload a file, status code: %d, content: %s", resp.StatusCode, string(body))
	}

	return nil
}

func init() {
	logrus.SetFormatter(&logrus.JSONFormatter{})
}

func main() {
	fVerbose = flag.Bool("v", false, "verbose")
	fLog = flag.Bool("log", false, "logging")
	fApiUrl = flag.String("api", "", "api base url")
	fToken = flag.String("token", "", "authentication token")
	fTask = flag.String("task", "", "supported types: uploadPackageSource")
	fPlugin = flag.String("plugin", "", "plugin name")
	fProject = flag.String("project", "", "project name")
	fEntityId = flag.String("entityId", "", "entity id")
	fAppId = flag.String("appId", "", "app id")
	fChunkSize = flag.Int64("chunkSize", 0, "chunk size")
	flag.Parse()

	if fVerbose != nil && *fVerbose {
		logrus.SetLevel(logrus.DebugLevel)
	}

	if fLog != nil && *fLog {
		f, err := os.OpenFile("metaverse-sdk-automation.log", os.O_WRONLY|os.O_CREATE, 0755)
		if err != nil {
			logrus.Fatalf("failed to open log file")
		}
		mw := io.MultiWriter(os.Stdout, f)
		logrus.SetOutput(mw)
	}

	if fApiUrl == nil {
		errorExit()
	}
	apiUrl = *fApiUrl
	if apiUrl == "" {
		errorExit()
	}

	if fToken == nil {
		errorExit()
	}
	token = *fToken
	if token == "" {
		errorExit()
	}

	if fEntityId == nil {
		errorExit()
	}

	entityId = uuid.FromStringOrNil(*fEntityId)
	if entityId.IsNil() {
		errorExit()
	}

	appId = uuid.FromStringOrNil(*fAppId)
	if appId.IsNil() {
		logrus.Warningf("no app id")
		//errorExit()
	}

	if fProject == nil {
		errorExit()
	}
	project = *fProject

	if fPlugin == nil {
		errorExit()
	}
	plugin = *fPlugin

	pluginDir, err := getPluginDir(project, plugin)
	if err != nil {
		logrus.Fatalf("failed to get plugin dir: %v", err)
	}

	pluginContentTempDir, err := getPluginTempDir(project, plugin)
	if err != nil {
		logrus.Fatalf("failed to get plugin temp dir: %v", err)
	}

	if fChunkSize != nil && *fChunkSize > minChunkSize {
		chunkSize = *fChunkSize
	} else {
		chunkSize = minChunkSize
	}

	if fTask == nil {
		errorExit()
	}
	task = *fTask
	switch task {
	case taskUploadPackageSource:
		{
			logrus.Debugf("uploading '%s' package descriptor", plugin)
			upluginName := filepath.Join(pluginDir, plugin+".uplugin")
			err = uploadEntityFile(entityId, "uplugin", "application/json", upluginName, plugin+".uplugin", nil)
			if err != nil {
				logrus.Fatalf("failed to upload entity file: %v", err)
			}

			logrus.Debugf("compressing '%s' package content", plugin)
			zipName := filepath.Join(pluginDir, plugin+".zip")
			zip, err := os.Create(zipName)
			if err != nil {
				logrus.Fatalf("failed to create a zip file: %v", err)
			}
			defer func(zip *os.File) {
				err := zip.Close()
				if err != nil {
					logrus.Errorf("failed to close a zip file: %v", err)
				}

				// delete zip file after upload
				err = os.Remove(zipName)
				if err != nil {
					logrus.Errorf("failed to delete zip file: %v", err)
				}
			}(zip)

			format := archiver.CompressedArchive{
				Archival: archiver.Zip{},
			}

			var archiveFileMap = map[string]string{}

			items, err := os.ReadDir(pluginContentTempDir)
			if err != nil {
				logrus.Fatalf("failed to read content dir: %v", err)
			}
			for _, item := range items {
				itemPath := filepath.Join(pluginContentTempDir, item.Name())
				archiveFileMap[itemPath] = ""
			}

			releaseArchiveFiles, err := archiver.FilesFromDisk(nil, archiveFileMap)
			if err != nil {
				logrus.Fatalf("failed to enumerate release archive files to zip: %v", err)
			}

			err = format.Archive(context.Background(), zip, releaseArchiveFiles)
			if err != nil {
				logrus.Fatalf("failed to zip release archive files: %v", err)
			}

			fi, err := zip.Stat()
			if err != nil {
				logrus.Fatalf("failed to get zip file info: %v", err)
			}
			zipSize := fi.Size()

			logrus.Debugf("uploading '%s' package content", plugin)

			//err = uploadEntityFile(entityId, "uplugin_content", "application/zip", zipName, plugin+".zip", nil)
			var presignedFileMetadata FileMetadata
			presignedFileMetadata, err = getEntityFileUploadUrl(entityId, "uplugin_content", "application/zip", zipSize, plugin+".zip")
			if err != nil {
				logrus.Fatalf("failed to get presigned upload file metadata: %v", err)
			}

			logrus.Debugf("uploading file %s", presignedFileMetadata.Id.String())

			//params := map[string]string{
			//	"version":      strconv.FormatInt(int64(presignedFileMetadata.Version), 10),
			//	"index":        strconv.FormatInt(int64(presignedFileMetadata.Index), 10),
			//	"type":         presignedFileMetadata.Type,
			//	"originalPath": presignedFileMetadata.OriginalPath,
			//}

			err = uploadEntityFileToS3(presignedFileMetadata.Url, entityId, zipName)
			if err != nil {
				logrus.Fatalf("failed to upload: %v", err)
			}

			err = createPackageJobs(entityId)
		}
	case taskUnzipPackageSource:
		{
			logrus.Debugf("unzip '%s' package content", plugin)
			zipName := filepath.Join(pluginDir, "temp", plugin+".zip")
			zip, err := os.Open(zipName)
			if err != nil {
				logrus.Fatalf("failed to open a zip file: %v", err)
			}
			defer func(zip *os.File) {
				err := zip.Close()
				if err != nil {
					logrus.Errorf("failed to close a zip file: %v", err)
				}
			}(zip)

			format := archiver.CompressedArchive{
				Archival: archiver.Zip{},
			}

			handler := func(ctx context.Context, f archiver.File) error {
				rc, err := f.Open()
				if err != nil {
					return err
				}
				defer rc.Close()

				if f.IsDir() {
					err = os.MkdirAll(filepath.Join(pluginDir, "Content", f.NameInArchive), f.Mode())
					if err == nil || os.IsExist(err) {
						return nil
					}
					return err
				}

				out, err := os.OpenFile(filepath.Join(pluginDir, "Content", f.NameInArchive), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
				if err != nil {
					return err
				}
				_, err = io.Copy(out, rc)
				return err
			}

			err = format.Extract(context.Background(), zip, nil, handler)
			if err != nil {
				logrus.Fatalf("failed to unzip release archive files: %v", err)
			}
		}
	//case taskUpdateSDK:
	//	{
	//		// Get current version of the SDK from the INI file.
	//		currentVersion, err := getProjectVersion(project)
	//		if err != nil {
	//			logrus.Fatalf("failed to get the current version: %v", err)
	//		}
	//		if currentVersion == nil {
	//			logrus.Fatalf("failed to get the current version")
	//		}
	//
	//		// Get the latest version from the API.
	//		latestVersion, err := getLatestVersion()
	//		if err != nil {
	//			logrus.Fatalf("failed to get the latest version: %v", err)
	//		}
	//		if latestVersion == nil {
	//			logrus.Fatalf("failed to get the latest version")
	//		}
	//
	//		// Check if the latest version greater than the current.
	//		if !currentVersion.LessThan(latestVersion) {
	//			logrus.Debugf("up to date")
	//			os.Exit(0)
	//		}
	//
	//		// 4. Download files.
	//		// 5. Replace files.
	//		// 6. Restart editor.
	//	}
	default:
		flag.Usage()
		logrus.Exit(-1)
	}
}
