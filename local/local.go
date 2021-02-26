package local

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buildpacks/imgutil/layer"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/pkg/errors"

	"github.com/buildpacks/imgutil"
)

type Image struct {
	docker           client.CommonAPIClient
	repoName         string
	platform         imgutil.Platform
	inspect          types.ImageInspect
	layerPaths       []string
	prevImage        *Image // reused layers will be fetched from prevImage
	downloadBaseOnce *sync.Once
}

type ImageOption func(*Image) (*Image, error)
type InitialImageOption ImageOption

func WithPreviousImage(imageName string) ImageOption {
	return func(i *Image) (*Image, error) {
		if _, err := inspectOptionalImage(i.docker, imageName, i.platform); err != nil {
			return i, err
		}

		prevImage, err := NewImage(imageName, i.docker, FromBaseImage(imageName))
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get previous image '%s'", imageName)
		}
		i.prevImage = prevImage

		return i, nil
	}
}

func FromBaseImage(imageName string) ImageOption {
	return func(i *Image) (*Image, error) {
		var (
			err     error
			inspect types.ImageInspect
		)

		if inspect, err = inspectOptionalImage(i.docker, imageName, i.platform); err != nil {
			return i, err
		}

		i.inspect = inspect
		i.layerPaths = make([]string, len(i.inspect.RootFS.Layers))

		return i, nil
	}
}

func WithPlatform(platform imgutil.Platform) InitialImageOption {
	return func(i *Image) (*Image, error) {
		if platform.OS != "" && platform.OS != i.inspect.Os {
			return nil, fmt.Errorf(`invalid os: platform os "%s" must match the daemon os "%s"`, platform.OS, i.inspect.Os)
		}

		i.inspect.Architecture = platform.Architecture
		i.inspect.OsVersion = platform.OSVersion

		i.platform = platform

		return i, nil
	}
}

func NewImage(repoName string, dockerClient client.CommonAPIClient, ops ...interface{}) (*Image, error) {
	var err error

	defaultPlatform, err := defaultPlatform(dockerClient)
	if err != nil {
		return nil, err
	}

	inspect := defaultInspect(defaultPlatform)

	image := &Image{
		docker:           dockerClient,
		repoName:         repoName,
		inspect:          inspect,
		platform:         defaultPlatform,
		layerPaths:       make([]string, len(inspect.RootFS.Layers)),
		downloadBaseOnce: &sync.Once{},
	}

	image, err = processImageOptions(image, ops)
	if err != nil {
		return nil, err
	}

	image, err = prepareImage(image)
	if err != nil {
		return nil, err
	}

	return image, nil
}

func processImageOptions(image *Image, ops []interface{}) (*Image, error) {
	sort.Slice(ops, func(i, _ int) bool {
		switch ops[i].(type) {
		case InitialImageOption:
			return true
		default:
			return false
		}
	})

	for _, op := range ops {
		var err error

		switch opFn := op.(type) {
		case InitialImageOption:
			image, err = opFn(image)
		case ImageOption:
			image, err = opFn(image)
		}
		if err != nil {
			return nil, err
		}
	}
	return image, nil
}

func prepareImage(image *Image) (*Image, error) {
	if image.inspect.Os != "windows" || len(image.inspect.RootFS.Layers) != 0 {
		return image, nil
	}

	layerReader, err := layer.WindowsBaseLayer()
	if err != nil {
		return nil, err
	}

	layerFile, err := ioutil.TempFile("", "imgutil.local.image.windowsbaselayer")
	if err != nil {
		return nil, errors.Wrap(err, "failed to create temp file")
	}
	defer layerFile.Close()

	hasher := sha256.New()

	multiWriter := io.MultiWriter(layerFile, hasher)

	if _, err := io.Copy(multiWriter, layerReader); err != nil {
		return nil, errors.Wrap(err, "failed to copy base layer")
	}

	diffID := "sha256:" + hex.EncodeToString(hasher.Sum(nil))

	image.inspect.RootFS.Layers = append(image.inspect.RootFS.Layers, diffID)
	image.layerPaths = append(image.layerPaths, layerFile.Name())

	return image, nil
}

func (i *Image) Label(key string) (string, error) {
	labels := i.inspect.Config.Labels
	return labels[key], nil
}

func (i *Image) Labels() (map[string]string, error) {
	copiedLabels := make(map[string]string)
	for i, l := range i.inspect.Config.Labels {
		copiedLabels[i] = l
	}
	return copiedLabels, nil
}

func (i *Image) Env(key string) (string, error) {
	for _, envVar := range i.inspect.Config.Env {
		parts := strings.Split(envVar, "=")
		if parts[0] == key {
			return parts[1], nil
		}
	}
	return "", nil
}

func (i *Image) Entrypoint() ([]string, error) {
	return i.inspect.Config.Entrypoint, nil
}

func (i *Image) OS() (string, error) {
	return i.inspect.Os, nil
}

func (i *Image) OSVersion() (string, error) {
	return i.inspect.OsVersion, nil
}

func (i *Image) Architecture() (string, error) {
	return i.inspect.Architecture, nil
}

func (i *Image) Rename(name string) {
	i.repoName = name
}

func (i *Image) Name() string {
	return i.repoName
}

func (i *Image) Found() bool {
	return i.inspect.ID != ""
}

func (i *Image) Identifier() (imgutil.Identifier, error) {
	return IDIdentifier{
		ImageID: strings.TrimPrefix(i.inspect.ID, "sha256:"),
	}, nil
}

func (i *Image) CreatedAt() (time.Time, error) {
	createdAtTime := i.inspect.Created
	createdTime, err := time.Parse(time.RFC3339Nano, createdAtTime)

	if err != nil {
		return time.Time{}, err
	}
	return createdTime, nil
}

func (i *Image) Rebase(baseTopLayer string, newBase imgutil.Image) error {
	ctx := context.Background()

	// FIND TOP LAYER
	var keepLayersIdx int
	for idx, diffID := range i.inspect.RootFS.Layers {
		if diffID == baseTopLayer {
			keepLayersIdx = idx + 1
			break
		}
	}
	if keepLayersIdx == 0 {
		return fmt.Errorf("'%s' not found in '%s' during rebase", baseTopLayer, i.repoName)
	}

	// DOWNLOAD IMAGE
	if err := i.downloadBaseLayersOnce(); err != nil {
		return err
	}

	// SWITCH BASE LAYERS
	newBaseInspect, _, err := i.docker.ImageInspectWithRaw(ctx, newBase.Name())
	if err != nil {
		return errors.Wrapf(err, "read config for new base image '%s'", newBase)
	}
	i.inspect.ID = newBaseInspect.ID
	i.downloadBaseOnce = &sync.Once{}
	i.inspect.RootFS.Layers = append(newBaseInspect.RootFS.Layers, i.inspect.RootFS.Layers[keepLayersIdx:]...)
	i.layerPaths = append(make([]string, len(newBaseInspect.RootFS.Layers)), i.layerPaths[keepLayersIdx:]...)
	return nil
}

func (i *Image) SetLabel(key, val string) error {
	if i.inspect.Config.Labels == nil {
		i.inspect.Config.Labels = map[string]string{}
	}

	i.inspect.Config.Labels[key] = val
	return nil
}

func (i *Image) SetOS(osVal string) error {
	if osVal != i.inspect.Os {
		return fmt.Errorf(`invalid os: must match the daemon: "%s"`, i.inspect.Os)
	}
	return nil
}

func (i *Image) SetOSVersion(osVersion string) error {
	i.inspect.OsVersion = osVersion
	return nil
}

func (i *Image) SetArchitecture(architecture string) error {
	i.inspect.Architecture = architecture
	return nil
}

func (i *Image) RemoveLabel(key string) error {
	delete(i.inspect.Config.Labels, key)
	return nil
}

func (i *Image) SetEnv(key, val string) error {
	ignoreCase := i.inspect.Os == "windows"
	for idx, kv := range i.inspect.Config.Env {
		parts := strings.SplitN(kv, "=", 2)
		foundKey := parts[0]
		searchKey := key
		if ignoreCase {
			foundKey = strings.ToUpper(foundKey)
			searchKey = strings.ToUpper(searchKey)
		}
		if foundKey == searchKey {
			i.inspect.Config.Env[idx] = fmt.Sprintf("%s=%s", key, val)
			return nil
		}
	}
	i.inspect.Config.Env = append(i.inspect.Config.Env, fmt.Sprintf("%s=%s", key, val))
	return nil
}

func (i *Image) SetWorkingDir(dir string) error {
	i.inspect.Config.WorkingDir = dir
	return nil
}

func (i *Image) SetEntrypoint(ep ...string) error {
	i.inspect.Config.Entrypoint = ep
	return nil
}

func (i *Image) SetCmd(cmd ...string) error {
	i.inspect.Config.Cmd = cmd
	return nil
}

func (i *Image) TopLayer() (string, error) {
	all := i.inspect.RootFS.Layers

	if len(all) == 0 {
		return "", fmt.Errorf("image '%s' has no layers", i.repoName)
	}

	topLayer := all[len(all)-1]
	return topLayer, nil
}

func (i *Image) GetLayer(diffID string) (io.ReadCloser, error) {
	for l := range i.inspect.RootFS.Layers {
		if i.inspect.RootFS.Layers[l] != diffID {
			continue
		}
		if i.layerPaths[l] == "" {
			if err := i.downloadBaseLayersOnce(); err != nil {
				return nil, err
			}
			if i.layerPaths[l] == "" {
				return nil, fmt.Errorf("failed to fetch layer '%s' from daemon", diffID)
			}
		}
		return os.Open(i.layerPaths[l])
	}

	return nil, fmt.Errorf("image '%s' does not contain layer with diff ID '%s'", i.repoName, diffID)
}

func (i *Image) AddLayer(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.Wrapf(err, "AddLayer: open layer: %s", path)
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return errors.Wrapf(err, "AddLayer: calculate checksum: %s", path)
	}
	diffID := "sha256:" + hex.EncodeToString(hasher.Sum(make([]byte, 0, hasher.Size())))
	return i.AddLayerWithDiffID(path, diffID)
}

func (i *Image) AddLayerWithDiffID(path, diffID string) error {
	i.inspect.RootFS.Layers = append(i.inspect.RootFS.Layers, diffID)
	i.layerPaths = append(i.layerPaths, path)
	return nil
}

func (i *Image) ReuseLayer(diffID string) error {
	if i.prevImage == nil {
		return errors.New("failed to reuse layer because no previous image was provided")
	}
	if !i.prevImage.Found() {
		return fmt.Errorf("failed to reuse layer because previous image '%s' was not found in daemon", i.prevImage.repoName)
	}

	if err := i.prevImage.downloadBaseLayersOnce(); err != nil {
		return err
	}

	for l := range i.prevImage.inspect.RootFS.Layers {
		if i.prevImage.inspect.RootFS.Layers[l] == diffID {
			return i.AddLayerWithDiffID(i.prevImage.layerPaths[l], diffID)
		}
	}
	return fmt.Errorf("SHA %s was not found in %s", diffID, i.prevImage.Name())
}

func (i *Image) Save(additionalNames ...string) error {
	// during the first save attempt some layers may be excluded. The docker daemon allows this if the given set
	// of layers already exists in the daemon in the given order
	inspect, err := i.doSave()
	if err != nil {
		// populate all layer paths and try again without the above performance optimization.
		if err := i.downloadBaseLayersOnce(); err != nil {
			return err
		}

		inspect, err = i.doSave()
		if err != nil {
			saveErr := imgutil.SaveError{}
			for _, n := range append([]string{i.Name()}, additionalNames...) {
				saveErr.Errors = append(saveErr.Errors, imgutil.SaveDiagnostic{ImageName: n, Cause: err})
			}
			return saveErr
		}
	}
	i.inspect = inspect

	var errs []imgutil.SaveDiagnostic
	for _, n := range append([]string{i.Name()}, additionalNames...) {
		if err := i.docker.ImageTag(context.Background(), i.inspect.ID, n); err != nil {
			errs = append(errs, imgutil.SaveDiagnostic{ImageName: n, Cause: err})
		}
	}

	if len(errs) > 0 {
		return imgutil.SaveError{Errors: errs}
	}

	return nil
}

func (i *Image) doSave() (types.ImageInspect, error) {
	ctx := context.Background()
	done := make(chan error)

	t, err := name.NewTag(i.repoName, name.WeakValidation)
	if err != nil {
		return types.ImageInspect{}, err
	}

	// returns valid 'name:tag' appending 'latest', if missing tag
	repoName := t.Name()

	pr, pw := io.Pipe()
	defer pw.Close()
	go func() {
		res, err := i.docker.ImageLoad(ctx, pr, true)
		if err != nil {
			done <- err
			return
		}

		// only return response error after response is drained and closed
		responseErr := checkResponseError(res.Body)
		drainCloseErr := ensureReaderClosed(res.Body)
		if responseErr != nil {
			done <- responseErr
			return
		}
		if drainCloseErr != nil {
			done <- drainCloseErr
		}

		done <- nil
	}()

	tw := tar.NewWriter(pw)
	defer tw.Close()

	configFile, err := i.newConfigFile()
	if err != nil {
		return types.ImageInspect{}, errors.Wrap(err, "generate config file")
	}

	id := fmt.Sprintf("%x", sha256.Sum256(configFile))
	if err := addTextToTar(tw, id+".json", configFile); err != nil {
		return types.ImageInspect{}, err
	}

	var layerPaths []string
	for _, path := range i.layerPaths {
		if path == "" {
			layerPaths = append(layerPaths, "")
			continue
		}
		layerName := fmt.Sprintf("/%x.tar", sha256.Sum256([]byte(path)))
		f, err := os.Open(path)
		if err != nil {
			return types.ImageInspect{}, err
		}
		defer f.Close()
		if err := addFileToTar(tw, layerName, f); err != nil {
			return types.ImageInspect{}, err
		}
		f.Close()
		layerPaths = append(layerPaths, layerName)
	}

	manifest, err := json.Marshal([]map[string]interface{}{
		{
			"Config":   id + ".json",
			"RepoTags": []string{repoName},
			"Layers":   layerPaths,
		},
	})
	if err != nil {
		return types.ImageInspect{}, err
	}

	if err := addTextToTar(tw, "manifest.json", manifest); err != nil {
		return types.ImageInspect{}, err
	}

	tw.Close()
	pw.Close()
	err = <-done
	if err != nil {
		return types.ImageInspect{}, errors.Wrapf(err, "image load '%s'. first error", i.repoName)
	}

	inspect, _, err := i.docker.ImageInspectWithRaw(context.Background(), id)
	if err != nil {
		if client.IsErrNotFound(err) {
			return types.ImageInspect{}, errors.Wrapf(err, "save image '%s'", i.repoName)
		}
		return types.ImageInspect{}, err
	}

	return inspect, nil
}

func (i *Image) newConfigFile() ([]byte, error) {
	cfg, err := v1Config(i.inspect)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cfg)
}

func (i *Image) Delete() error {
	if !i.Found() {
		return nil
	}
	options := types.ImageRemoveOptions{
		Force:         true,
		PruneChildren: true,
	}
	_, err := i.docker.ImageRemove(context.Background(), i.inspect.ID, options)
	return err
}

func (i *Image) ManifestSize() (int64, error) {
	return 0, nil
}

// downloadBaseLayersOnce exports the base image from the daemon and populates layerPaths the first time it is called.
// subsequent calls do nothing.
func (i *Image) downloadBaseLayersOnce() error {
	var err error
	if !i.Found() {
		return nil
	}
	i.downloadBaseOnce.Do(func() {
		err = i.downloadBaseLayers()
	})
	if err != nil {
		return errors.Wrap(err, "failed to fetch base layers")
	}
	return err
}

func (i *Image) downloadBaseLayers() error {
	ctx := context.Background()

	imageReader, err := i.docker.ImageSave(ctx, []string{i.inspect.ID})
	if err != nil {
		return errors.Wrapf(err, "failed to save base image with ID '%s' from the docker daemon", i.inspect.ID)
	}
	defer ensureReaderClosed(imageReader)

	tmpDir, err := ioutil.TempDir("", "imgutil.local.image.")
	if err != nil {
		return errors.Wrap(err, "failed to create temp dir")
	}

	err = untar(imageReader, tmpDir)
	if err != nil {
		return err
	}

	mf, err := os.Open(filepath.Join(tmpDir, "manifest.json"))
	if err != nil {
		return err
	}
	defer mf.Close()

	var manifest []struct {
		Config string
		Layers []string
	}
	if err := json.NewDecoder(mf).Decode(&manifest); err != nil {
		return err
	}

	if len(manifest) != 1 {
		return fmt.Errorf("manifest.json had unexpected number of entries: %d", len(manifest))
	}

	df, err := os.Open(filepath.Join(tmpDir, manifest[0].Config))
	if err != nil {
		return err
	}
	defer df.Close()

	var details struct {
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}

	if err = json.NewDecoder(df).Decode(&details); err != nil {
		return err
	}

	for l := range details.RootFS.DiffIDs {
		i.layerPaths[l] = filepath.Join(tmpDir, manifest[0].Layers[l])
	}

	for l := range i.layerPaths {
		if i.layerPaths[l] == "" {
			return errors.New("failed to download all base layers from daemon")
		}
	}

	return nil
}

func addTextToTar(tw *tar.Writer, name string, contents []byte) error {
	hdr := &tar.Header{Name: name, Mode: 0644, Size: int64(len(contents))}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(contents)
	return err
}

func addFileToTar(tw *tar.Writer, name string, contents *os.File) error {
	fi, err := contents.Stat()
	if err != nil {
		return err
	}
	hdr := &tar.Header{Name: name, Mode: 0644, Size: fi.Size()}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, contents)
	return err
}

func untar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// end of tar archive
			return nil
		}
		if err != nil {
			return err
		}

		path := filepath.Join(dest, hdr.Name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, hdr.FileInfo().Mode()); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			_, err := os.Stat(filepath.Dir(path))
			if os.IsNotExist(err) {
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					return err
				}
			}

			fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, hdr.FileInfo().Mode())
			if err != nil {
				return err
			}
			if _, err := io.Copy(fh, tr); err != nil {
				fh.Close()
				return err
			}
			fh.Close()
		case tar.TypeSymlink:
			_, err := os.Stat(filepath.Dir(path))
			if os.IsNotExist(err) {
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					return err
				}
			}

			if err := os.Symlink(hdr.Linkname, path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown file type in tar %d", hdr.Typeflag)
		}
	}
}

func inspectOptionalImage(docker client.CommonAPIClient, imageName string, platform imgutil.Platform) (types.ImageInspect, error) {
	var (
		err     error
		inspect types.ImageInspect
	)

	if inspect, _, err = docker.ImageInspectWithRaw(context.Background(), imageName); err != nil {
		if client.IsErrNotFound(err) {
			return defaultInspect(platform), nil
		}

		return types.ImageInspect{}, errors.Wrapf(err, "verifying image '%s'", imageName)
	}

	return inspect, nil
}

func defaultInspect(platform imgutil.Platform) types.ImageInspect {
	return types.ImageInspect{
		Os:           platform.OS,
		Architecture: platform.Architecture,
		OsVersion:    platform.OSVersion,
		Config:       &container.Config{},
	}
}

func defaultPlatform(dockerClient client.CommonAPIClient) (imgutil.Platform, error) {
	daemonInfo, err := dockerClient.Info(context.Background())
	if err != nil {
		return imgutil.Platform{}, err
	}

	return imgutil.Platform{
		OS:           daemonInfo.OSType,
		Architecture: "amd64",
	}, nil
}

func v1Config(inspect types.ImageInspect) (v1.ConfigFile, error) {
	history := make([]v1.History, len(inspect.RootFS.Layers))
	for i := range history {
		// zero history
		history[i] = v1.History{
			Created: v1.Time{Time: imgutil.NormalizedDateTime},
		}
	}
	diffIDs := make([]v1.Hash, len(inspect.RootFS.Layers))
	for i, layer := range inspect.RootFS.Layers {
		hash, err := v1.NewHash(layer)
		if err != nil {
			return v1.ConfigFile{}, err
		}
		diffIDs[i] = hash
	}
	exposedPorts := make(map[string]struct{}, len(inspect.Config.ExposedPorts))
	for key, val := range inspect.Config.ExposedPorts {
		exposedPorts[string(key)] = val
	}
	var config v1.Config
	if inspect.Config != nil {
		var healthcheck *v1.HealthConfig
		if inspect.Config.Healthcheck != nil {
			healthcheck = &v1.HealthConfig{
				Test:        inspect.Config.Healthcheck.Test,
				Interval:    inspect.Config.Healthcheck.Interval,
				Timeout:     inspect.Config.Healthcheck.Timeout,
				StartPeriod: inspect.Config.Healthcheck.StartPeriod,
				Retries:     inspect.Config.Healthcheck.Retries,
			}
		}
		config = v1.Config{
			AttachStderr:    inspect.Config.AttachStderr,
			AttachStdin:     inspect.Config.AttachStdin,
			AttachStdout:    inspect.Config.AttachStdout,
			Cmd:             inspect.Config.Cmd,
			Healthcheck:     healthcheck,
			Domainname:      inspect.Config.Domainname,
			Entrypoint:      inspect.Config.Entrypoint,
			Env:             inspect.Config.Env,
			Hostname:        inspect.Config.Hostname,
			Image:           inspect.Config.Image,
			Labels:          inspect.Config.Labels,
			OnBuild:         inspect.Config.OnBuild,
			OpenStdin:       inspect.Config.OpenStdin,
			StdinOnce:       inspect.Config.StdinOnce,
			Tty:             inspect.Config.Tty,
			User:            inspect.Config.User,
			Volumes:         inspect.Config.Volumes,
			WorkingDir:      inspect.Config.WorkingDir,
			ExposedPorts:    exposedPorts,
			ArgsEscaped:     inspect.Config.ArgsEscaped,
			NetworkDisabled: inspect.Config.NetworkDisabled,
			MacAddress:      inspect.Config.MacAddress,
			StopSignal:      inspect.Config.StopSignal,
			Shell:           inspect.Config.Shell,
		}
	}
	return v1.ConfigFile{
		Architecture: inspect.Architecture,
		Created:      v1.Time{Time: imgutil.NormalizedDateTime},
		History:      history,
		OS:           inspect.Os,
		OSVersion:    inspect.OsVersion,
		RootFS: v1.RootFS{
			Type:    "layers",
			DiffIDs: diffIDs,
		},
		Config: config,
	}, nil
}

func checkResponseError(r io.Reader) error {
	decoder := json.NewDecoder(r)
	var jsonMessage jsonmessage.JSONMessage
	if err := decoder.Decode(&jsonMessage); err != nil {
		return errors.Wrapf(err, "parsing daemon response")
	}

	if jsonMessage.Error != nil {
		return errors.Wrap(jsonMessage.Error, "embedded daemon response")
	}
	return nil
}

// ensureReaderClosed drains and closes and reader, returning the first error
func ensureReaderClosed(r io.ReadCloser) error {
	_, err := io.Copy(ioutil.Discard, r)
	if closeErr := r.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}
