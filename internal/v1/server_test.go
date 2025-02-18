package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/osbuild/image-builder/internal/common"
	"github.com/osbuild/image-builder/internal/composer"
	"github.com/osbuild/image-builder/internal/db"
	"github.com/osbuild/image-builder/internal/distribution"
	"github.com/osbuild/image-builder/internal/logger"
	"github.com/osbuild/image-builder/internal/provisioning"
	"github.com/osbuild/image-builder/internal/tutils"

	"github.com/labstack/echo/v4"
)

var dbc *tutils.PSQLContainer

func TestMain(m *testing.M) {
	code := runTests(m)
	os.Exit(code)
}

func runTests(m *testing.M) int {
	d, err := tutils.NewPSQLContainer()
	if err != nil {
		panic(err)
	}

	dbc = d
	code := m.Run()
	defer func() {
		err = dbc.Stop()
		if err != nil {
			logrus.Errorf("Error stopping postgres container: %v", err)
		}
	}()
	return code
}

// Create a temporary file containing quotas, returns the file name as a string
func initQuotaFile(t *testing.T) (string, error) {
	// create quotas with only the default values
	quotas := map[string]common.Quota{
		"default": {Quota: common.DefaultQuota, SlidingWindow: common.DefaultSlidingWindow},
	}
	jsonQuotas, err := json.Marshal(quotas)
	if err != nil {
		return "", err
	}

	// get a temp file to store the quotas
	file, err := os.CreateTemp(t.TempDir(), "account_quotas.*.json")
	if err != nil {
		return "", err
	}

	// write to disk
	jsonFile, err := os.Create(file.Name())
	if err != nil {
		fmt.Println(err)
		return "", err
	}
	_, err = jsonFile.Write(jsonQuotas)
	if err != nil {
		return "", err
	}
	err = jsonFile.Close()
	if err != nil {
		return "", err
	}
	return file.Name(), nil
}

func makeUploadOptions(t *testing.T, uploadOptions interface{}) *composer.UploadOptions {
	data, err := json.Marshal(uploadOptions)
	require.NoError(t, err)

	var result composer.UploadOptions
	err = json.Unmarshal(data, &result)
	require.NoError(t, err)

	return &result
}

func startServerWithCustomDB(t *testing.T, url, provURL string, dbase db.DB, distsDir string, allowFile string) (*echo.Echo, *httptest.Server) {
	var log = &logrus.Logger{
		Out:       os.Stderr,
		Formatter: new(logrus.TextFormatter),
		Hooks:     make(logrus.LevelHooks),
		Level:     logrus.DebugLevel,
	}

	err := logger.ConfigLogger(log, "DEBUG")
	require.NoError(t, err)

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "rhsm-api", r.FormValue("client_id"))
		require.Equal(t, "offlinetoken", r.FormValue("refresh_token"))
		require.Equal(t, "refresh_token", r.FormValue("grant_type"))

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(struct {
			AccessToken string `json:"access_token"`
		}{
			AccessToken: "accesstoken",
		})
		require.NoError(t, err)
	}))

	compClient, err := composer.NewClient(composer.ComposerClientConfig{
		ComposerURL:  url,
		TokenURL:     tokenServer.URL,
		ClientId:     "rhsm-api",
		OfflineToken: "offlinetoken",
	})
	require.NoError(t, err)

	provClient, err := provisioning.NewClient(provisioning.ProvisioningClientConfig{
		URL: provURL,
	})
	require.NoError(t, err)

	//store the quotas in a temporary file
	quotaFile, err := initQuotaFile(t)
	require.NoError(t, err)

	adr, err := distribution.LoadDistroRegistry(distsDir)
	require.NoError(t, err)

	echoServer := echo.New()
	echoServer.HideBanner = true
	serverConfig := &ServerConfig{
		EchoServer: echoServer,
		CompClient: compClient,
		ProvClient: provClient,
		DBase:      dbase,
		QuotaFile:  quotaFile,
		AllowFile:  allowFile,
		AllDistros: adr,
	}

	err = Attach(serverConfig)
	require.NoError(t, err)
	// execute in parallel b/c .Run() will block execution
	go func() {
		_ = echoServer.Start("localhost:8086")
	}()

	// wait until server is ready
	tries := 0
	for tries < 5 {
		resp, err := tutils.GetResponseError("http://localhost:8086/status")
		if err == nil {
			defer resp.Body.Close()
		}
		if err == nil && resp.StatusCode == http.StatusOK {
			break
		} else if tries == 4 {
			require.NoError(t, err)
		}
		time.Sleep(time.Second)
		tries += 1
	}

	return echoServer, tokenServer
}

func startServer(t *testing.T, url, provURL string) (*echo.Echo, *httptest.Server) {
	dbase, err := dbc.NewDB()
	require.NoError(t, err)
	return startServerWithCustomDB(t, url, provURL, dbase, "../../distributions", "")
}

func startServerWithAllowFile(t *testing.T, url, provURL, string, distsDir string, allowFile string) (*echo.Echo, *httptest.Server) {
	dbase, err := dbc.NewDB()
	require.NoError(t, err)
	return startServerWithCustomDB(t, url, provURL, dbase, distsDir, allowFile)
}

// note: all of the sub-tests below don't actually talk to
// osbuild-composer API that's why they are groupped together
func TestWithoutOsbuildComposerBackend(t *testing.T) {
	// note: any url will work, it'll only try to contact the osbuild-composer
	// instance when calling /compose or /compose/$uuid
	srv, tokenSrv := startServer(t, "", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	t.Run("VerifyIdentityHeaderMissing", func(t *testing.T) {
		respStatusCode, body := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/version", nil)
		require.Equal(t, 400, respStatusCode)
		require.Contains(t, body, "missing x-rh-identity header")
	})

	t.Run("GetVersion", func(t *testing.T) {
		respStatusCode, body := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/version", &tutils.AuthString0)
		require.Equal(t, 200, respStatusCode)

		var result Version
		err := json.Unmarshal([]byte(body), &result)
		require.NoError(t, err)
		require.Equal(t, "1.0", result.Version)
	})

	t.Run("GetOpenapiJson", func(t *testing.T) {
		respStatusCode, _ := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/openapi.json", &tutils.AuthString0)
		require.Equal(t, 200, respStatusCode)
		// note: not asserting body b/c response is too big
	})

	t.Run("GetDistributions", func(t *testing.T) {
		respStatusCode, body := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/distributions", &tutils.AuthString0)
		require.Equal(t, 200, respStatusCode)

		var result DistributionsResponse
		err := json.Unmarshal([]byte(body), &result)
		require.NoError(t, err)

		for _, distro := range result {
			require.Contains(t, []string{"rhel-8", "rhel-8-nightly", "rhel-84", "rhel-85", "rhel-86", "rhel-87", "rhel-88", "rhel-9", "rhel-9-nightly", "rhel-90", "rhel-91", "rhel-92", "centos-8", "centos-9", "fedora-35", "fedora-36", "fedora-37", "fedora-38", "fedora-39"}, distro.Name)
		}
	})

	t.Run("GetArchitectures", func(t *testing.T) {
		respStatusCode, body := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/architectures/centos-8", &tutils.AuthString0)
		require.Equal(t, 200, respStatusCode)

		var result Architectures
		err := json.Unmarshal([]byte(body), &result)
		require.NoError(t, err)
		require.Equal(t, Architectures{
			ArchitectureItem{
				Arch:       "x86_64",
				ImageTypes: []string{"aws", "gcp", "azure", "ami", "vhd", "guest-image", "image-installer", "vsphere", "vsphere-ova"},
				Repositories: []Repository{
					{
						Baseurl: common.StringToPtr("http://mirror.centos.org/centos/8-stream/BaseOS/x86_64/os/"),
						Rhsm:    false,
					}, {
						Baseurl: common.StringToPtr("http://mirror.centos.org/centos/8-stream/AppStream/x86_64/os/"),
						Rhsm:    false,
					}, {
						Baseurl: common.StringToPtr("http://mirror.centos.org/centos/8-stream/extras/x86_64/os/"),
						Rhsm:    false,
					},
				},
			},
			ArchitectureItem{
				Arch:       "aarch64",
				ImageTypes: []string{"aws", "guest-image", "image-installer"},
				Repositories: []Repository{
					{
						Baseurl: common.StringToPtr("http://mirror.centos.org/centos/8-stream/BaseOS/aarch64/os/"),
						Rhsm:    false,
					}, {
						Baseurl: common.StringToPtr("http://mirror.centos.org/centos/8-stream/AppStream/aarch64/os/"),
						Rhsm:    false,
					}, {
						Baseurl: common.StringToPtr("http://mirror.centos.org/centos/8-stream/extras/aarch64/os/"),
						Rhsm:    false,
					},
				},
			}}, result)
	})

	t.Run("GetPackages", func(t *testing.T) {
		architectures := []string{"x86_64", "aarch64"}
		for _, arch := range architectures {
			respStatusCode, body := tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/packages?distribution=rhel-8&architecture=%s&search=ssh", arch), &tutils.AuthString0)
			require.Equal(t, 200, respStatusCode)

			var result PackagesResponse
			err := json.Unmarshal([]byte(body), &result)
			require.NoError(t, err)
			require.Contains(t, result.Data[0].Name, "ssh")
			require.Greater(t, result.Meta.Count, 0)
			require.Contains(t, result.Links.First, "search=ssh")
			p1 := result.Data[0]
			p2 := result.Data[1]

			respStatusCode, body = tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/packages?distribution=rhel-8&architecture=%s&search=4e3086991b3f452d82eed1f2122aefeb", arch), &tutils.AuthString0)
			require.Equal(t, 200, respStatusCode)
			err = json.Unmarshal([]byte(body), &result)
			require.NoError(t, err)
			require.Empty(t, result.Data)
			require.Contains(t, body, "\"data\":[]")

			respStatusCode, body = tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/packages?offset=121039&distribution=rhel-8&architecture=%s&search=4e3086991b3f452d82eed1f2122aefeb", arch), &tutils.AuthString0)
			require.Equal(t, 200, respStatusCode)
			err = json.Unmarshal([]byte(body), &result)
			require.NoError(t, err)
			require.Empty(t, result.Data)
			require.Equal(t, fmt.Sprintf("/api/image-builder/v1.0/packages?search=4e3086991b3f452d82eed1f2122aefeb&distribution=rhel-8&architecture=%s&offset=0&limit=100", arch), result.Links.First)
			require.Equal(t, fmt.Sprintf("/api/image-builder/v1.0/packages?search=4e3086991b3f452d82eed1f2122aefeb&distribution=rhel-8&architecture=%s&offset=0&limit=100", arch), result.Links.Last)

			respStatusCode, body = tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/packages?distribution=rhel-8&architecture=%s&search=ssh&limit=1", arch), &tutils.AuthString0)
			require.Equal(t, 200, respStatusCode)
			err = json.Unmarshal([]byte(body), &result)
			require.NoError(t, err)
			require.Greater(t, result.Meta.Count, 1)
			require.Equal(t, result.Data[0], p1)

			respStatusCode, body = tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/packages?distribution=rhel-8&architecture=%s&search=ssh&limit=1&offset=1", arch), &tutils.AuthString0)
			require.Equal(t, 200, respStatusCode)
			err = json.Unmarshal([]byte(body), &result)
			require.NoError(t, err)
			require.Greater(t, result.Meta.Count, 1)
			require.Equal(t, result.Data[0], p2)

			respStatusCode, _ = tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/packages?distribution=rhel-8&architecture=%s&search=ssh&limit=-13", arch), &tutils.AuthString0)
			require.Equal(t, 400, respStatusCode)
			respStatusCode, _ = tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/packages?distribution=rhel-8&architecture=%s&search=ssh&limit=13&offset=-2193", arch), &tutils.AuthString0)
			require.Equal(t, 400, respStatusCode)

			respStatusCode, _ = tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/packages?distribution=none&architecture=%s&search=ssh", arch), &tutils.AuthString0)
			require.Equal(t, 400, respStatusCode)
		}
	})

	t.Run("AccountNumberFallback", func(t *testing.T) {
		respStatusCode, _ := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/version", &tutils.AuthString0WithoutEntitlements)
		require.Equal(t, 200, respStatusCode)
	})

	t.Run("BogusAuthString", func(t *testing.T) {
		auth := "notbase64"
		respStatusCode, body := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/version", &auth)
		require.Equal(t, 400, respStatusCode)
		require.Contains(t, body, "unable to b64 decode x-rh-identity header")
	})

	t.Run("BogusBase64AuthString", func(t *testing.T) {
		auth := "dGhpcyBpcyBkZWZpbml0ZWx5IG5vdCBqc29uCg=="
		respStatusCode, body := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/version", &auth)
		require.Equal(t, 400, respStatusCode)
		require.Contains(t, body, "does not contain valid JSON")
	})

	t.Run("EmptyAccountNumber", func(t *testing.T) {
		// AccoundNumber equals ""
		auth := tutils.GetCompleteBase64Header("000000")
		respStatusCode, _ := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/version", &auth)
		require.Equal(t, 200, respStatusCode)
	})

	t.Run("EmptyOrgID", func(t *testing.T) {
		// OrgID equals ""
		auth := tutils.GetCompleteBase64Header("")
		respStatusCode, body := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/version", &auth)
		require.Equal(t, 400, respStatusCode)
		require.Contains(t, body, "invalid or missing org_id")
	})

	t.Run("StatusCheck", func(t *testing.T) {
		respStatusCode, _ := tutils.GetResponseBody(t, "http://localhost:8086/status", nil)
		require.Equal(t, 200, respStatusCode)
	})
}

func TestOrgIdWildcard(t *testing.T) {
	srv, tokenSrv := startServer(t, "", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	t.Run("Authorized", func(t *testing.T) {
		respStatusCode, _ := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/version", &tutils.AuthString0)
		require.Equal(t, 200, respStatusCode)
	})
}

func TestAccountNumberWildcard(t *testing.T) {
	srv, tokenSrv := startServer(t, "", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	t.Run("Authorized", func(t *testing.T) {
		respStatusCode, _ := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/version", &tutils.AuthString0)
		require.Equal(t, 200, respStatusCode)
	})
}

// note: this scenario needs to talk to a simulated osbuild-composer API
func TestGetComposeStatus(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		s := composer.ComposeStatus{
			ImageStatus: composer.ImageStatus{
				Status: composer.ImageStatusValueBuilding,
			},
		}
		err := json.NewEncoder(w).Encode(s)
		require.NoError(t, err)
	}))
	defer apiSrv.Close()

	dbase, err := dbc.NewDB()
	require.NoError(t, err)
	imageName := "MyImageName"
	id := uuid.New()
	err = dbase.InsertCompose(id, "600000", "000001", &imageName, json.RawMessage(`
		{
			"distribution": "rhel-9",
			"image_requests": [],
			"image_name": "myimage"
		}`))
	require.NoError(t, err)

	srv, tokenSrv := startServerWithCustomDB(t, apiSrv.URL, "", dbase, "../../distributions", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	respStatusCode, body := tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/composes/%s",
		id), &tutils.AuthString1)
	require.Equal(t, 200, respStatusCode)

	var result ComposeStatus
	err = json.Unmarshal([]byte(body), &result)
	require.NoError(t, err)
	require.Equal(t, ComposeStatus{
		ImageStatus: ImageStatus{
			Status: "building",
		},
		Request: ComposeRequest{
			Distribution:  "rhel-9",
			ImageName:     common.StringToPtr("myimage"),
			ImageRequests: []ImageRequest{},
		},
	}, result)

	respStatusCode, body = tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/composes/%s",
		id), &tutils.AuthString1)
	require.Equal(t, 200, respStatusCode)
	err = json.Unmarshal([]byte(body), &result)
	require.NoError(t, err)
	require.Equal(t, ComposeStatus{
		ImageStatus: ImageStatus{
			Status: "building",
		},
		Request: ComposeRequest{
			Distribution:  "rhel-9",
			ImageName:     common.StringToPtr("myimage"),
			ImageRequests: []ImageRequest{},
		},
	}, result)
}

// note: this scenario needs to talk to a simulated osbuild-composer API
func TestGetComposeStatus404(t *testing.T) {
	id := uuid.New().String()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "404 during tests")
	}))
	defer apiSrv.Close()

	srv, tokenSrv := startServer(t, apiSrv.URL, "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	respStatusCode, body := tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/composes/%s",
		id), &tutils.AuthString0)
	require.Equal(t, 404, respStatusCode)
	require.Contains(t, body, "Compose not found")
}

func TestGetComposeMetadata(t *testing.T) {
	id := uuid.New()
	testPackages := []composer.PackageMetadata{
		{
			Arch:      "ArchTest2",
			Epoch:     strptr("EpochTest2"),
			Name:      "NameTest2",
			Release:   "ReleaseTest2",
			Sigmd5:    "Sigmd5Test2",
			Signature: strptr("SignatureTest2"),
			Type:      "TypeTest2",
			Version:   "VersionTest2",
		},
		{
			Arch:      "ArchTest1",
			Epoch:     strptr("EpochTest1"),
			Name:      "NameTest1",
			Release:   "ReleaseTest1",
			Sigmd5:    "Sigmd5Test1",
			Signature: strptr("SignatureTest1"),
			Type:      "TypeTest1",
			Version:   "VersionTest1",
		},
	}
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		m := composer.ComposeMetadata{
			OstreeCommit: strptr("test string"),
			Packages:     &testPackages,
		}

		err := json.NewEncoder(w).Encode(m)
		require.NoError(t, err)
	}))
	defer apiSrv.Close()

	dbase, err := dbc.NewDB()
	require.NoError(t, err)
	imageName := "MyImageName"
	err = dbase.InsertCompose(id, "500000", "000000", &imageName, json.RawMessage("{}"))
	require.NoError(t, err)

	srv, tokenSrv := startServerWithCustomDB(t, apiSrv.URL, "", dbase, "../../distributions", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	var result composer.ComposeMetadata

	// Get API response and compare
	respStatusCode, body := tutils.GetResponseBody(t,
		fmt.Sprintf("http://localhost:8086/api/image-builder/v1/composes/%s/metadata", id), &tutils.AuthString0)
	require.Equal(t, 200, respStatusCode)
	err = json.Unmarshal([]byte(body), &result)
	require.NoError(t, err)
	require.Equal(t, *result.Packages, testPackages)
}

func TestGetComposeMetadata404(t *testing.T) {
	id := uuid.New().String()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "404 during tests")
	}))
	defer apiSrv.Close()

	srv, tokenSrv := startServer(t, apiSrv.URL, "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	respStatusCode, body := tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/composes/%s/metadata",
		id), &tutils.AuthString0)
	require.Equal(t, 404, respStatusCode)
	require.Contains(t, body, "Compose not found")
}

func TestGetComposes(t *testing.T) {
	id := uuid.New()
	id2 := uuid.New()
	id3 := uuid.New()
	dbase, err := dbc.NewDB()
	require.NoError(t, err)

	db_srv, tokenSrv := startServerWithCustomDB(t, "", "", dbase, "../../distributions", "")
	defer func() {
		err := db_srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	var result ComposesResponse
	respStatusCode, body := tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/composes", &tutils.AuthString0)

	require.Equal(t, 200, respStatusCode)
	err = json.Unmarshal([]byte(body), &result)
	require.NoError(t, err)
	require.Equal(t, 0, result.Meta.Count)
	require.Contains(t, body, "\"data\":[]")

	imageName := "MyImageName"
	err = dbase.InsertCompose(id, "500000", "000000", &imageName, json.RawMessage("{}"))
	require.NoError(t, err)
	err = dbase.InsertCompose(id2, "500000", "000000", &imageName, json.RawMessage("{}"))
	require.NoError(t, err)
	err = dbase.InsertCompose(id3, "500000", "000000", &imageName, json.RawMessage("{}"))
	require.NoError(t, err)

	composeEntry, err := dbase.GetCompose(id, "000000")
	require.NoError(t, err)

	respStatusCode, body = tutils.GetResponseBody(t, "http://localhost:8086/api/image-builder/v1/composes", &tutils.AuthString0)
	require.Equal(t, 200, respStatusCode)
	err = json.Unmarshal([]byte(body), &result)
	require.NoError(t, err)
	require.Equal(t, 3, result.Meta.Count)
	require.Equal(t, composeEntry.CreatedAt.Format(time.RFC3339), result.Data[2].CreatedAt)
	require.Equal(t, composeEntry.Id, result.Data[2].Id)
}

// note: these scenarios don't needs to talk to a simulated osbuild-composer API
func TestComposeImage(t *testing.T) {
	// note: any url will work, it'll only try to contact the osbuild-composer
	// instance when calling /compose or /compose/$uuid
	srv, tokenSrv := startServer(t, "", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	t.Run("ErrorsForZeroImageRequests", func(t *testing.T) {
		payload := ComposeRequest{
			Customizations: nil,
			Distribution:   "centos-8",
			ImageRequests:  []ImageRequest{},
		}
		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
		require.Equal(t, 400, respStatusCode)
		require.Contains(t, body, `Error at \"/image_requests\": minimum number of items is 1`)
	})

	t.Run("ErrorsForTwoImageRequests", func(t *testing.T) {
		payload := ComposeRequest{
			Customizations: nil,
			Distribution:   "centos-8",
			ImageRequests: []ImageRequest{
				{
					Architecture: "x86_64",
					ImageType:    ImageTypesAws,
					UploadRequest: UploadRequest{
						Type: UploadTypesAws,
						Options: AWSUploadRequestOptions{
							ShareWithAccounts: &[]string{"test-account"},
						},
					},
				},
				{
					Architecture: "x86_64",
					ImageType:    ImageTypesAmi,
					UploadRequest: UploadRequest{
						Type: UploadTypesAws,
						Options: AWSUploadRequestOptions{
							ShareWithAccounts: &[]string{"test-account"},
						},
					},
				},
			},
		}
		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
		require.Equal(t, 400, respStatusCode)
		require.Contains(t, body, `Error at \"/image_requests\": maximum number of items is 1`)
	})

	t.Run("ErrorsForEmptyAccountsAndSources", func(t *testing.T) {
		payload := ComposeRequest{
			Customizations: nil,
			Distribution:   "centos-8",
			ImageRequests: []ImageRequest{
				{
					Architecture: "x86_64",
					ImageType:    ImageTypesAws,
					UploadRequest: UploadRequest{
						Type:    UploadTypesAws,
						Options: AWSUploadRequestOptions{},
					},
				},
			},
		}
		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
		require.Equal(t, 400, respStatusCode)
		require.Contains(t, body, "Expected at least one source or account to share the image with")
	})

	azureRequest := func(source_id, subscription_id, tenant_id string) ImageRequest {
		options := make(map[string]string)
		options["resource_group"] = "group"
		if source_id != "" {
			options["source_id"] = source_id
		}
		if subscription_id != "" {
			options["subscription_id"] = subscription_id
		}
		if tenant_id != "" {
			options["tenant_id"] = tenant_id
		}
		optionsJSON, _ := json.Marshal(options)

		var azureOptions AzureUploadRequestOptions
		err := json.Unmarshal(optionsJSON, &azureOptions)
		require.NoError(t, err)

		azureRequest := ImageRequest{
			Architecture: "x86_64",
			ImageType:    ImageTypesAzure,
			UploadRequest: UploadRequest{
				Type:    UploadTypesAzure,
				Options: azureOptions,
			},
		}

		return azureRequest
	}

	azureTests := []struct {
		name    string
		request ImageRequest
	}{
		{name: "AzureInvalid1", request: azureRequest("", "", "")},
		{name: "AzureInvalid2", request: azureRequest("", "1", "")},
		{name: "AzureInvalid3", request: azureRequest("", "", "1")},
		{name: "AzureInvalid4", request: azureRequest("1", "1", "")},
		{name: "AzureInvalid5", request: azureRequest("1", "", "1")},
		{name: "AzureInvalid6", request: azureRequest("1", "1", "1")},
	}

	for _, tc := range azureTests {
		t.Run(tc.name, func(t *testing.T) {
			payload := ComposeRequest{
				Customizations: nil,
				Distribution:   "centos-8",
				ImageRequests:  []ImageRequest{tc.request},
			}
			respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
			require.Equal(t, 400, respStatusCode)
			require.Contains(t, body, "Request must contain either (1) a source id, and no tenant or subscription ids or (2) tenant and subscription ids, and no source id.")
		})
	}

	t.Run("ErrorsForZeroUploadRequests", func(t *testing.T) {
		payload := ComposeRequest{
			Customizations: nil,
			Distribution:   "centos-8",
			ImageRequests: []ImageRequest{
				{
					Architecture:  "x86_64",
					ImageType:     ImageTypesAzure,
					UploadRequest: UploadRequest{},
				},
			},
		}
		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
		require.Equal(t, 400, respStatusCode)
		require.Regexp(t, "image_requests/0/upload_request/options|image_requests/0/upload_request/type", body)
		require.Regexp(t, "Value is not nullable|value is not one of the allowed values", body)
	})

	t.Run("ISEWhenRepositoriesNotFound", func(t *testing.T) {
		// Distro arch isn't supported which triggers error when searching
		// for repositories
		payload := ComposeRequest{
			Customizations: nil,
			Distribution:   "centos-8",
			ImageRequests: []ImageRequest{
				{
					Architecture: "unsupported-arch",
					ImageType:    ImageTypesAws,
					UploadRequest: UploadRequest{
						Type: UploadTypesAws,
						Options: AWSUploadRequestOptions{
							ShareWithAccounts: &[]string{"test-account"},
						},
					},
				},
			},
		}
		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
		require.Equal(t, 400, respStatusCode)
		require.Contains(t, body, "Error at \\\"/image_requests/0/architecture\\\"")
	})

	t.Run("ErrorUserCustomizationNotAllowed", func(t *testing.T) {
		// User customization only permitted for installer types
		payload := ComposeRequest{
			Customizations: &Customizations{
				Packages: &[]string{
					"some",
					"packages",
				},
				Users: &[]User{
					{
						Name:   "user-name0",
						SshKey: "",
					},
					{
						Name:   "user-name1",
						SshKey: "",
					},
				},
			},
			Distribution: "centos-8",
			ImageRequests: []ImageRequest{
				{
					Architecture: "x86_64",
					ImageType:    ImageTypesAmi,
					UploadRequest: UploadRequest{
						Type: UploadTypesAws,
						Options: AWSUploadRequestOptions{
							ShareWithAccounts: &[]string{"test-account"},
						},
					},
				},
			},
		}
		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
		require.Equal(t, 400, respStatusCode)
		require.Contains(t, body, "User customization only applies to installer image types")
	})

	t.Run("ErrorsForUnknownUploadType", func(t *testing.T) {
		// UploadRequest Type isn't supported
		payload := ComposeRequest{
			Customizations: nil,
			Distribution:   "centos-8",
			ImageRequests: []ImageRequest{
				{
					Architecture: "x86_64",
					ImageType:    ImageTypesAzure,
					UploadRequest: UploadRequest{
						Type: "unknown",
						Options: AWSUploadRequestOptions{
							ShareWithAccounts: &[]string{"test-account"},
						},
					},
				},
			},
		}
		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
		require.Equal(t, 400, respStatusCode)
		require.Contains(t, body, "Error at \\\"/image_requests/0/upload_request/type\\\"")
	})

	t.Run("ErrorMaxSizeForAWSAndAzure", func(t *testing.T) {
		// 66 GiB total
		payload := ComposeRequest{
			Customizations: &Customizations{
				Filesystem: &[]Filesystem{
					{
						Mountpoint: "/",
						MinSize:    2147483648,
					},
					{
						Mountpoint: "/var",
						MinSize:    68719476736,
					},
				},
			},
			Distribution: "centos-8",
			ImageRequests: []ImageRequest{
				{
					Architecture:  "x86_64",
					ImageType:     ImageTypesAmi,
					UploadRequest: UploadRequest{},
				},
			},
		}

		awsUr := UploadRequest{
			Type: UploadTypesAws,
			Options: AWSUploadRequestOptions{
				ShareWithAccounts: &[]string{"test-account"},
			},
		}

		azureUr := UploadRequest{
			Type: UploadTypesAzure,
			Options: AzureUploadRequestOptions{
				ResourceGroup:  "group",
				SubscriptionId: strptr("id"),
				TenantId:       strptr("tenant"),
				ImageName:      strptr("azure-image"),
			},
		}
		for _, it := range []ImageTypes{ImageTypesAmi, ImageTypesAws} {
			payload.ImageRequests[0].ImageType = it
			payload.ImageRequests[0].UploadRequest = awsUr
			respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
			require.Equal(t, 400, respStatusCode)
			require.Contains(t, body, fmt.Sprintf("Total AWS image size cannot exceed %d bytes", FSMaxSize))
		}

		for _, it := range []ImageTypes{ImageTypesAzure, ImageTypesVhd} {
			payload.ImageRequests[0].ImageType = it
			payload.ImageRequests[0].UploadRequest = azureUr
			respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
			require.Equal(t, 400, respStatusCode)
			require.Contains(t, body, fmt.Sprintf("Total Azure image size cannot exceed %d bytes", FSMaxSize))
		}
	})
}

func TestComposeImageErrorsWhenStatusCodeIsNotStatusCreated(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		s := "deliberately returning !201 during tests"
		err := json.NewEncoder(w).Encode(s)
		require.NoError(t, err)
	}))
	defer apiSrv.Close()

	srv, tokenSrv := startServer(t, apiSrv.URL, "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	payload := ComposeRequest{
		Customizations: nil,
		Distribution:   "centos-8",
		ImageRequests: []ImageRequest{
			{
				Architecture: "x86_64",
				ImageType:    ImageTypesAws,
				UploadRequest: UploadRequest{
					Type: UploadTypesAws,
					Options: AWSUploadRequestOptions{
						ShareWithAccounts: &[]string{"test-account"},
					},
				},
			},
		},
	}
	respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
	require.Equal(t, http.StatusInternalServerError, respStatusCode)
	require.Contains(t, body, "Failed posting compose request to osbuild-composer")
}

func TestComposeImageErrorResolvingOSTree(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		serviceStat := &composer.Error{
			Id:     "10",
			Reason: "not ok",
		}
		err := json.NewEncoder(w).Encode(serviceStat)
		require.NoError(t, err)
	}))
	defer apiSrv.Close()

	srv, tokenSrv := startServer(t, apiSrv.URL, "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	payload := ComposeRequest{
		Customizations: &Customizations{
			Packages: nil,
		},
		Distribution: "centos-8",
		ImageRequests: []ImageRequest{
			{
				Architecture: "x86_64",
				ImageType:    ImageTypesEdgeCommit,
				Ostree: &OSTree{
					Ref: strptr("edge/ref"),
				},
				UploadRequest: UploadRequest{
					Type: UploadTypesAwsS3,
					Options: AWSUploadRequestOptions{
						ShareWithAccounts: &[]string{"test-account"},
					},
				},
			},
		},
	}
	respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
	require.Equal(t, 400, respStatusCode)
	require.Contains(t, body, "Error resolving OSTree repo")
}

func TestComposeImageErrorsWhenCannotParseResponse(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		s := "not a composer.ComposeId data structure"
		err := json.NewEncoder(w).Encode(s)
		require.NoError(t, err)
	}))
	defer apiSrv.Close()

	srv, tokenSrv := startServer(t, apiSrv.URL, "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	payload := ComposeRequest{
		Customizations: nil,
		Distribution:   "centos-8",
		ImageRequests: []ImageRequest{
			{
				Architecture: "x86_64",
				ImageType:    ImageTypesAws,
				UploadRequest: UploadRequest{
					Type: UploadTypesAws,
					Options: AWSUploadRequestOptions{
						ShareWithAccounts: &[]string{"test-account"},
					},
				},
			},
		},
	}
	respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
	require.Equal(t, 500, respStatusCode)
	require.Contains(t, body, "Internal Server Error")
}

func TestComposeImageReturnsIdWhenNoErrors(t *testing.T) {
	id := uuid.New()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		result := composer.ComposeId{
			Id: id,
		}
		err := json.NewEncoder(w).Encode(result)
		require.NoError(t, err)
	}))
	defer apiSrv.Close()

	srv, tokenSrv := startServer(t, apiSrv.URL, "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	payload := ComposeRequest{
		Customizations: nil,
		Distribution:   "centos-8",
		ImageRequests: []ImageRequest{
			{
				Architecture: "x86_64",
				ImageType:    ImageTypesAws,
				UploadRequest: UploadRequest{
					Type: UploadTypesAws,
					Options: AWSUploadRequestOptions{
						ShareWithAccounts: &[]string{"test-account"},
					},
				},
			},
		},
	}
	respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
	require.Equal(t, http.StatusCreated, respStatusCode)

	var result ComposeResponse
	err := json.Unmarshal([]byte(body), &result)
	require.NoError(t, err)
	require.Equal(t, id, result.Id)
}

func TestComposeImageAllowList(t *testing.T) {
	distsDir := "../distribution/testdata/distributions"
	allowFile := "../common/testdata/allow.json"
	id := uuid.New()

	createApiSrv := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if "Bearer" == r.Header.Get("Authorization") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			result := composer.ComposeId{
				Id: id,
			}
			err := json.NewEncoder(w).Encode(result)
			require.NoError(t, err)
		}))
	}

	createPayload := func(distro Distributions) ComposeRequest {
		return ComposeRequest{
			Customizations: nil,
			Distribution:   distro,
			ImageRequests: []ImageRequest{
				{
					Architecture: "x86_64",
					ImageType:    ImageTypesAws,
					UploadRequest: UploadRequest{
						Type: UploadTypesAws,
						Options: AWSUploadRequestOptions{
							ShareWithAccounts: &[]string{"test-account"},
						},
					},
				},
			},
		}
	}

	t.Run("restricted distribution, allowed", func(t *testing.T) {
		apiSrv := createApiSrv()
		defer apiSrv.Close()

		srv, tokenSrv := startServerWithAllowFile(t, apiSrv.URL, "", "", distsDir, allowFile)
		defer func() {
			err := srv.Shutdown(context.Background())
			require.NoError(t, err)
		}()
		defer tokenSrv.Close()

		payload := createPayload("centos-8")

		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
		require.Equal(t, http.StatusCreated, respStatusCode)

		var result ComposeResponse
		err := json.Unmarshal([]byte(body), &result)
		require.NoError(t, err)
		require.Equal(t, id, result.Id)
	})

	t.Run("restricted distribution, forbidden", func(t *testing.T) {
		apiSrv := createApiSrv()
		defer apiSrv.Close()

		srv, tokenSrv := startServerWithAllowFile(t, apiSrv.URL, "", "", distsDir, allowFile)
		defer func() {
			err := srv.Shutdown(context.Background())
			require.NoError(t, err)
		}()
		defer tokenSrv.Close()

		payload := createPayload("rhel-8")

		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
		require.Equal(t, http.StatusForbidden, respStatusCode)

		var result ComposeResponse
		err := json.Unmarshal([]byte(body), &result)
		require.NoError(t, err)
		require.Equal(t, uuid.Nil, result.Id)
	})

	t.Run("restricted distribution, forbidden (no allowFile)", func(t *testing.T) {
		apiSrv := createApiSrv()
		defer apiSrv.Close()

		srv, tokenSrv := startServerWithAllowFile(t, apiSrv.URL, "", "", distsDir, "")
		defer func() {
			err := srv.Shutdown(context.Background())
			require.NoError(t, err)
		}()
		defer tokenSrv.Close()

		payload := createPayload("centos-8")

		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload)
		require.Equal(t, http.StatusForbidden, respStatusCode)

		var result ComposeResponse
		err := json.Unmarshal([]byte(body), &result)
		require.NoError(t, err)
		require.Equal(t, uuid.Nil, result.Id)
	})
}

// convenience function for string pointer fields
func strptr(s string) *string {
	return &s
}

func TestComposeCustomizations(t *testing.T) {
	var id uuid.UUID
	var composerRequest composer.ComposeRequest
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))

		err := json.NewDecoder(r.Body).Decode(&composerRequest)
		require.NoError(t, err)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		id = uuid.New()
		result := composer.ComposeId{
			Id: id,
		}
		err = json.NewEncoder(w).Encode(result)
		require.NoError(t, err)
	}))
	defer apiSrv.Close()

	awsAccountId := "123456123456"
	provSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result provisioning.V1SourceUploadInfoResponse

		if r.URL.Path == "/sources/1/upload_info" {
			awsId := struct {
				AccountId *string `json:"account_id,omitempty"`
			}{
				AccountId: &awsAccountId,
			}
			result.Aws = &awsId
		}

		if r.URL.Path == "/sources/2/upload_info" {
			azureInfo := struct {
				ResourceGroups *[]string `json:"resource_groups,omitempty"`
				SubscriptionId *string   `json:"subscription_id,omitempty"`
				TenantId       *string   `json:"tenant_id,omitempty"`
			}{
				SubscriptionId: strptr("id"),
				TenantId:       strptr("tenant"),
				ResourceGroups: &[]string{"group"},
			}
			result.Azure = &azureInfo
		}

		require.Equal(t, tutils.AuthString0, r.Header.Get("x-rh-identity"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		err := json.NewEncoder(w).Encode(result)
		require.NoError(t, err)
	}))

	srv, tokenSrv := startServer(t, apiSrv.URL, provSrv.URL)
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	payloads := []struct {
		imageBuilderRequest ComposeRequest
		composerRequest     composer.ComposeRequest
	}{
		{
			imageBuilderRequest: ComposeRequest{
				Customizations: &Customizations{
					Packages: &[]string{
						"some",
						"packages",
					},
					PayloadRepositories: &[]Repository{
						{
							Baseurl:      common.StringToPtr("https://some-repo-base-url.org"),
							CheckGpg:     common.BoolToPtr(true),
							CheckRepoGpg: common.BoolToPtr(true),
							Gpgkey:       common.StringToPtr("some-gpg-key"),
							IgnoreSsl:    common.BoolToPtr(false),
							Rhsm:         false,
						},
					},
					Filesystem: &[]Filesystem{
						{
							Mountpoint: "/",
							MinSize:    2147483648,
						},
						{
							Mountpoint: "/var",
							MinSize:    1073741824,
						},
					},
					Users: &[]User{
						{
							Name:   "user",
							SshKey: "ssh-rsa AAAAB3NzaC1",
						},
					},
					CustomRepositories: &[]CustomRepository{
						{
							Id:       "some-repo-id",
							Baseurl:  &[]string{"https://some-repo-base-url.org"},
							Gpgkey:   &[]string{"some-gpg-key"},
							CheckGpg: common.BoolToPtr(true),
						},
					},
					Openscap: &OpenSCAP{
						ProfileId: "test-profile",
					},
				},
				Distribution: "centos-8",
				ImageRequests: []ImageRequest{
					{
						Architecture: "x86_64",
						ImageType:    ImageTypesRhelEdgeInstaller,
						UploadRequest: UploadRequest{
							Type:    UploadTypesAwsS3,
							Options: AWSS3UploadRequestOptions{},
						},
					},
				},
			},
			composerRequest: composer.ComposeRequest{
				Distribution: "centos-8",
				Customizations: &composer.Customizations{
					Packages: &[]string{
						"some",
						"packages",
					},
					PayloadRepositories: &[]composer.Repository{
						{
							Baseurl:      common.StringToPtr("https://some-repo-base-url.org"),
							CheckGpg:     common.BoolToPtr(true),
							CheckRepoGpg: common.BoolToPtr(true),
							Gpgkey:       common.StringToPtr("some-gpg-key"),
							IgnoreSsl:    common.BoolToPtr(false),
							Rhsm:         common.BoolToPtr(false),
						},
					},
					Filesystem: &[]composer.Filesystem{
						{
							Mountpoint: "/",
							MinSize:    2147483648,
						},
						{
							Mountpoint: "/var",
							MinSize:    1073741824,
						},
					},
					Users: &[]composer.User{
						{
							Name:   "user",
							Key:    common.StringToPtr("ssh-rsa AAAAB3NzaC1"),
							Groups: &[]string{"wheel"},
						},
					},
					CustomRepositories: &[]composer.CustomRepository{
						{
							Id:       "some-repo-id",
							Baseurl:  &[]string{"https://some-repo-base-url.org"},
							Gpgkey:   &[]string{"some-gpg-key"},
							CheckGpg: common.BoolToPtr(true),
						},
					},
					Openscap: &composer.OpenSCAP{
						ProfileId: "test-profile",
					},
				},
				ImageRequest: &composer.ImageRequest{
					Architecture: "x86_64",
					ImageType:    composer.ImageTypesEdgeInstaller,
					Ostree:       nil,
					Repositories: []composer.Repository{

						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/BaseOS/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/AppStream/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/extras/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
					},
					UploadOptions: makeUploadOptions(t, composer.AWSS3UploadOptions{
						Region: "",
					}),
				},
			},
		},
		{
			imageBuilderRequest: ComposeRequest{
				Customizations: &Customizations{
					Packages: nil,
				},
				Distribution: "rhel-8",
				ImageRequests: []ImageRequest{
					{
						Architecture: "x86_64",
						ImageType:    ImageTypesEdgeCommit,
						Ostree: &OSTree{
							Ref: strptr("edge/ref"),
						},
						UploadRequest: UploadRequest{
							Type:    UploadTypesAwsS3,
							Options: AWSS3UploadRequestOptions{},
						},
					},
				},
			},
			composerRequest: composer.ComposeRequest{
				Distribution: "rhel-88",
				Customizations: &composer.Customizations{
					Packages: nil,
				},
				ImageRequest: &composer.ImageRequest{
					Architecture: "x86_64",
					ImageType:    composer.ImageTypesEdgeCommit,
					Ostree: &composer.OSTree{
						Ref: strptr("edge/ref"),
					},
					Repositories: []composer.Repository{
						{
							Baseurl:     common.StringToPtr("https://cdn.redhat.com/content/dist/rhel8/8.8/x86_64/baseos/os"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(true),
						},
						{
							Baseurl:     common.StringToPtr("https://cdn.redhat.com/content/dist/rhel8/8.8/x86_64/appstream/os"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(true),
						},
					},
					UploadOptions: makeUploadOptions(t, composer.AWSS3UploadOptions{
						Region: "",
					}),
				},
			},
		},
		{
			imageBuilderRequest: ComposeRequest{
				Customizations: &Customizations{
					Packages: &[]string{"pkg"},
					Subscription: &Subscription{
						Organization: 000,
					},
				},
				Distribution: "centos-8",
				ImageRequests: []ImageRequest{
					{
						Architecture: "x86_64",
						ImageType:    ImageTypesRhelEdgeCommit,
						Ostree: &OSTree{
							Ref:        strptr("test/edge/ref"),
							Url:        strptr("https://ostree.srv/"),
							Contenturl: strptr("https://ostree.srv/content"),
							Parent:     strptr("test/edge/ref2"),
							Rhsm:       common.BoolToPtr(true),
						},
						UploadRequest: UploadRequest{
							Type:    UploadTypesAwsS3,
							Options: AWSS3UploadRequestOptions{},
						},
					},
				},
			},
			composerRequest: composer.ComposeRequest{
				Distribution: "centos-8",
				Customizations: &composer.Customizations{
					Packages: &[]string{
						"pkg",
					},
					Subscription: &composer.Subscription{
						ActivationKey: "",
						BaseUrl:       "",
						Insights:      false,
						Rhc:           common.BoolToPtr(false),
						Organization:  "0",
						ServerUrl:     "",
					},
				},
				ImageRequest: &composer.ImageRequest{
					Architecture: "x86_64",
					ImageType:    composer.ImageTypesEdgeCommit,
					Ostree: &composer.OSTree{
						Ref:        strptr("test/edge/ref"),
						Url:        strptr("https://ostree.srv/"),
						Contenturl: strptr("https://ostree.srv/content"),
						Parent:     strptr("test/edge/ref2"),
						Rhsm:       common.BoolToPtr(true),
					},
					Repositories: []composer.Repository{

						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/BaseOS/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/AppStream/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/extras/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
					},
					UploadOptions: makeUploadOptions(t, composer.AWSS3UploadOptions{
						Region: "",
					}),
				},
			},
		},
		// Test Azure with SubscriptionId and TenantId
		{
			imageBuilderRequest: ComposeRequest{
				Distribution: "centos-8",
				ImageRequests: []ImageRequest{
					{
						Architecture: "x86_64",
						ImageType:    ImageTypesAzure,
						Ostree: &OSTree{
							Ref:    strptr("test/edge/ref"),
							Url:    strptr("https://ostree.srv/"),
							Parent: strptr("test/edge/ref2"),
						},
						UploadRequest: UploadRequest{
							Type: UploadTypesAzure,
							Options: AzureUploadRequestOptions{
								ResourceGroup:  "group",
								SubscriptionId: strptr("id"),
								TenantId:       strptr("tenant"),
								ImageName:      strptr("azure-image"),
							},
						},
					},
				},
			},
			composerRequest: composer.ComposeRequest{
				Distribution:   "centos-8",
				Customizations: nil,
				ImageRequest: &composer.ImageRequest{
					Architecture: "x86_64",
					ImageType:    composer.ImageTypesAzure,
					Ostree: &composer.OSTree{
						Ref:    strptr("test/edge/ref"),
						Url:    strptr("https://ostree.srv/"),
						Parent: strptr("test/edge/ref2"),
					},
					Repositories: []composer.Repository{

						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/BaseOS/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/AppStream/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/extras/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
					},
					UploadOptions: makeUploadOptions(t, composer.AzureUploadOptions{
						ImageName:      strptr("azure-image"),
						ResourceGroup:  "group",
						SubscriptionId: "id",
						TenantId:       "tenant",
					}),
				},
			},
		},
		// Test Azure with SourceId
		{
			imageBuilderRequest: ComposeRequest{
				Distribution: "centos-8",
				ImageRequests: []ImageRequest{
					{
						Architecture: "x86_64",
						ImageType:    ImageTypesAzure,
						Ostree: &OSTree{
							Ref:    strptr("test/edge/ref"),
							Url:    strptr("https://ostree.srv/"),
							Parent: strptr("test/edge/ref2"),
						},
						UploadRequest: UploadRequest{
							Type: UploadTypesAzure,
							Options: AzureUploadRequestOptions{
								ResourceGroup: "group",
								SourceId:      strptr("2"),
								ImageName:     strptr("azure-image"),
							},
						},
					},
				},
			},
			composerRequest: composer.ComposeRequest{
				Distribution:   "centos-8",
				Customizations: nil,
				ImageRequest: &composer.ImageRequest{
					Architecture: "x86_64",
					ImageType:    composer.ImageTypesAzure,
					Ostree: &composer.OSTree{
						Ref:    strptr("test/edge/ref"),
						Url:    strptr("https://ostree.srv/"),
						Parent: strptr("test/edge/ref2"),
					},
					Repositories: []composer.Repository{

						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/BaseOS/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/AppStream/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/extras/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
					},
					UploadOptions: makeUploadOptions(t, composer.AzureUploadOptions{
						ImageName:      strptr("azure-image"),
						ResourceGroup:  "group",
						SubscriptionId: "id",
						TenantId:       "tenant",
					}),
				},
			},
		},
		{
			imageBuilderRequest: ComposeRequest{
				Distribution: "centos-8",
				ImageRequests: []ImageRequest{
					{
						Architecture: "x86_64",
						ImageType:    ImageTypesAws,
						UploadRequest: UploadRequest{
							Type: UploadTypesAws,
							Options: AWSUploadRequestOptions{
								ShareWithSources: &[]string{"1"},
							},
						},
					},
				},
			},
			composerRequest: composer.ComposeRequest{
				Distribution:   "centos-8",
				Customizations: nil,
				ImageRequest: &composer.ImageRequest{
					Architecture: "x86_64",
					ImageType:    composer.ImageTypesAws,
					Repositories: []composer.Repository{

						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/BaseOS/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/AppStream/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
						{
							Baseurl:     common.StringToPtr("http://mirror.centos.org/centos/8-stream/extras/x86_64/os/"),
							CheckGpg:    nil,
							Gpgkey:      nil,
							IgnoreSsl:   nil,
							Metalink:    nil,
							Mirrorlist:  nil,
							PackageSets: nil,
							Rhsm:        common.BoolToPtr(false),
						},
					},
					UploadOptions: makeUploadOptions(t, composer.AWSEC2UploadOptions{
						ShareWithAccounts: []string{awsAccountId},
					}),
				},
			},
		},
	}

	for idx, payload := range payloads {
		fmt.Printf("TT payload %d\n", idx)
		respStatusCode, body := tutils.PostResponseBody(t, "http://localhost:8086/api/image-builder/v1/compose", payload.imageBuilderRequest)
		require.Equal(t, http.StatusCreated, respStatusCode)

		var result ComposeResponse
		err := json.Unmarshal([]byte(body), &result)
		require.NoError(t, err)
		require.Equal(t, id, result.Id)

		//compare expected compose request with actual receieved compose request
		require.Equal(t, payload.composerRequest, composerRequest)
		composerRequest = composer.ComposeRequest{}
	}
}

// TestBuildOSTreeOptions checks if the buildOSTreeOptions utility function
// properly transfers the ostree options to the Composer structure.
func TestBuildOSTreeOptions(t *testing.T) {
	cases := map[ImageRequest]*composer.OSTree{
		{Ostree: nil}: nil,
		{Ostree: &OSTree{Ref: strptr("someref")}}:                                     {Ref: strptr("someref")},
		{Ostree: &OSTree{Ref: strptr("someref"), Url: strptr("https://example.org")}}: {Ref: strptr("someref"), Url: strptr("https://example.org")},
		{Ostree: &OSTree{Url: strptr("https://example.org")}}:                         {Url: strptr("https://example.org")},
	}

	for in, expOut := range cases {
		require.Equal(t, expOut, buildOSTreeOptions(in.Ostree), "input: %#v", in)
	}
}

func TestReadinessProbeNotReady(t *testing.T) {
	srv, tokenSrv := startServer(t, "", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	respStatusCode, _ := tutils.GetResponseBody(t, "http://localhost:8086/ready", &tutils.AuthString0)
	require.NotEqual(t, 200, respStatusCode)
	require.NotEqual(t, 404, respStatusCode)
}

func TestReadinessProbeReady(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "{\"version\":\"fake\"}")
	}))
	defer apiSrv.Close()

	srv, tokenSrv := startServer(t, apiSrv.URL, "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	respStatusCode, body := tutils.GetResponseBody(t, "http://localhost:8086/ready", &tutils.AuthString0)
	require.Equal(t, 200, respStatusCode)
	require.Contains(t, body, "{\"readiness\":\"ready\"}")
}

func TestMetrics(t *testing.T) {
	srv, tokenSrv := startServer(t, "", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	respStatusCode, body := tutils.GetResponseBody(t, "http://localhost:8086/metrics", nil)
	require.Equal(t, 200, respStatusCode)
	require.Contains(t, body, "image_builder_crc_compose_requests_total")
	require.Contains(t, body, "image_builder_crc_compose_errors")
}

func TestComposeStatusError(t *testing.T) {
	id := uuid.New()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer accesstoken", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")

		//nolint
		var manifestErrorDetails interface{}
		manifestErrorDetails = []composer.ComposeStatusError{
			composer.ComposeStatusError{
				Id:     23,
				Reason: "Marking errors: package",
			},
		}

		//nolint
		var osbuildErrorDetails interface{}
		osbuildErrorDetails = []composer.ComposeStatusError{
			composer.ComposeStatusError{
				Id:      5,
				Reason:  "dependency failed",
				Details: &manifestErrorDetails,
			},
		}

		s := composer.ComposeStatus{
			ImageStatus: composer.ImageStatus{
				Status: composer.ImageStatusValueFailure,
				Error: &composer.ComposeStatusError{
					Id:      9,
					Reason:  "depenceny failed",
					Details: &osbuildErrorDetails,
				},
			},
		}

		err := json.NewEncoder(w).Encode(s)
		require.NoError(t, err)
	}))
	defer apiSrv.Close()

	dbase, err := dbc.NewDB()
	require.NoError(t, err)
	imageName := "MyImageName"
	err = dbase.InsertCompose(id, "600000", "000001", &imageName, json.RawMessage("{}"))
	require.NoError(t, err)

	srv, tokenSrv := startServerWithCustomDB(t, apiSrv.URL, "", dbase, "../../distributions", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	respStatusCode, body := tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/composes/%s",
		id), &tutils.AuthString1)
	require.Equal(t, 200, respStatusCode)

	var result ComposeStatus
	err = json.Unmarshal([]byte(body), &result)
	require.NoError(t, err)
	require.Equal(t, ComposeStatus{
		ImageStatus: ImageStatus{
			Status: "failure",
			Error: &ComposeStatusError{
				Id:     23,
				Reason: "Marking errors: package",
			},
		},
		Request: ComposeRequest{},
	}, result)

}

func TestGetClones(t *testing.T) {
	id := uuid.New()
	cloneId := uuid.New()
	awsAccountId := "123456123456"

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)

		var cloneReq composer.AWSEC2CloneCompose
		err := json.NewDecoder(r.Body).Decode(&cloneReq)
		require.NoError(t, err)
		require.Equal(t, awsAccountId, (*cloneReq.ShareWithAccounts)[0])

		result := composer.CloneComposeResponse{
			Id: cloneId,
		}
		err = json.NewEncoder(w).Encode(result)
		require.NoError(t, err)
	}))
	defer apiSrv.Close()

	provSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		awsId := struct {
			AccountId *string `json:"account_id,omitempty"`
		}{
			AccountId: &awsAccountId,
		}
		result := provisioning.V1SourceUploadInfoResponse{
			Aws: &awsId,
		}

		require.Equal(t, tutils.AuthString0, r.Header.Get("x-rh-identity"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		err := json.NewEncoder(w).Encode(result)
		require.NoError(t, err)
	}))
	defer provSrv.Close()

	dbase, err := dbc.NewDB()
	require.NoError(t, err)
	err = dbase.InsertCompose(id, "500000", "000000", nil, json.RawMessage(`
{
  "image_requests": [
    {
      "image_type": "aws"
    }
  ]
}`))
	require.NoError(t, err)
	srv, tokenSrv := startServerWithCustomDB(t, apiSrv.URL, provSrv.URL, dbase, "../../distributions", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	var csResp ClonesResponse
	respStatusCode, body := tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/composes/%s/clones", id), &tutils.AuthString0)
	require.Equal(t, http.StatusOK, respStatusCode)
	err = json.Unmarshal([]byte(body), &csResp)
	require.NoError(t, err)
	require.Equal(t, 0, len(csResp.Data))
	require.Contains(t, body, "\"data\":[]")

	cloneReq := AWSEC2Clone{
		Region:           "us-east-2",
		ShareWithSources: &[]string{"1"},
	}
	respStatusCode, body = tutils.PostResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/composes/%s/clone", id), cloneReq)
	require.Equal(t, http.StatusCreated, respStatusCode)

	var cResp CloneResponse
	err = json.Unmarshal([]byte(body), &cResp)
	require.NoError(t, err)
	require.Equal(t, cloneId, cResp.Id)

	respStatusCode, body = tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/composes/%s/clones", id), &tutils.AuthString0)
	require.Equal(t, http.StatusOK, respStatusCode)
	err = json.Unmarshal([]byte(body), &csResp)
	require.NoError(t, err)
	require.Equal(t, 1, len(csResp.Data))
	require.Equal(t, cloneId, csResp.Data[0].Id)

	cloneReqExp, err := json.Marshal(cloneReq)
	require.NoError(t, err)
	cloneReqRecv, err := json.Marshal(csResp.Data[0].Request)
	require.NoError(t, err)
	require.Equal(t, cloneReqExp, cloneReqRecv)
}

func TestGetCloneStatus(t *testing.T) {
	cloneId := uuid.New()
	id := uuid.New()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if "Bearer" == r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, fmt.Sprintf("/clones/%v", cloneId)) && r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			usO := composer.AWSEC2UploadStatus{
				Ami:    "ami-1",
				Region: "us-east-2",
			}
			result := composer.CloneStatus{
				Options: usO,
				Status:  composer.Success,
				Type:    composer.UploadTypesAws,
			}
			err := json.NewEncoder(w).Encode(result)
			require.NoError(t, err)
		} else if strings.HasSuffix(r.URL.Path, fmt.Sprintf("%v/clone", id)) && r.Method == "POST" {
			w.WriteHeader(http.StatusCreated)
			result := composer.CloneComposeResponse{
				Id: cloneId,
			}
			err := json.NewEncoder(w).Encode(result)
			require.NoError(t, err)
		} else {
			require.FailNowf(t, "Unexpected request to mocked composer, path: %s", r.URL.Path)
		}
	}))
	defer apiSrv.Close()

	dbase, err := dbc.NewDB()
	require.NoError(t, err)
	err = dbase.InsertCompose(id, "500000", "000000", nil, json.RawMessage(`
{
  "image_requests": [
    {
      "image_type": "aws"
    }
  ]
}`))
	require.NoError(t, err)
	srv, tokenSrv := startServerWithCustomDB(t, apiSrv.URL, "", dbase, "../../distributions", "")
	defer func() {
		err := srv.Shutdown(context.Background())
		require.NoError(t, err)
	}()
	defer tokenSrv.Close()

	cloneReq := AWSEC2Clone{
		Region: "us-east-2",
	}
	respStatusCode, body := tutils.PostResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/composes/%s/clone", id), cloneReq)
	require.Equal(t, http.StatusCreated, respStatusCode)

	var cResp CloneResponse
	err = json.Unmarshal([]byte(body), &cResp)
	require.NoError(t, err)
	require.Equal(t, cloneId, cResp.Id)

	var usResp UploadStatus
	respStatusCode, body = tutils.GetResponseBody(t, fmt.Sprintf("http://localhost:8086/api/image-builder/v1/clones/%s", cloneId), &tutils.AuthString0)

	require.Equal(t, http.StatusOK, respStatusCode)
	err = json.Unmarshal([]byte(body), &usResp)
	require.NoError(t, err)
	require.Equal(t, UploadStatusStatusSuccess, usResp.Status)
	require.Equal(t, UploadTypesAws, usResp.Type)

	var awsUS AWSUploadStatus
	jsonUO, err := json.Marshal(usResp.Options)
	require.NoError(t, err)
	err = json.Unmarshal(jsonUO, &awsUS)
	require.NoError(t, err)
	require.Equal(t, "ami-1", awsUS.Ami)
	require.Equal(t, "us-east-2", awsUS.Region)
}

func TestValidateSpec(t *testing.T) {
	spec, err := GetSwagger()
	require.NoError(t, err)
	err = spec.Validate(context.Background())
	require.NoError(t, err)
}
