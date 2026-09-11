package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListModelsSupportsOpenAIAndGeminiAuthentication(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "modelstestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headerName     string
		expectedObject string
		expectedField  string
	}{
		{
			name:           "OpenAI bearer token",
			path:           "/v1/models",
			headerName:     "Authorization",
			expectedObject: "list",
			expectedField:  "data",
		},
		{
			name:          "Gemini API key header",
			path:          "/v1/models",
			headerName:    "x-goog-api-key",
			expectedField: "models",
		},
		{
			name:          "Gemini API key query",
			path:          "/v1/models?key=modelstestkey",
			expectedField: "models",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.headerName != "" {
				value := "modelstestkey"
				if test.headerName == "Authorization" {
					value = "Bearer " + value
				}
				request.Header.Set(test.headerName, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.expectedField)
			assert.NotContains(t, payload, "error")
			if test.expectedObject != "" {
				assert.Equal(t, test.expectedObject, payload["object"])
			}
		})
	}
}

func TestRelayPrivacyFilterCoversHostAndPluginRoutes(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Task{}))
	t.Setenv("TRUSTED_PROXIES", "")

	privacySetting := operation_setting.GetPrivacyFilterSetting()
	previousPrivacySetting := *privacySetting
	previousRegistry := jsplugin.DefaultRegistry
	previousRateLimit := setting.ModelRequestRateLimitEnabled
	t.Cleanup(func() {
		*privacySetting = previousPrivacySetting
		jsplugin.DefaultRegistry = previousRegistry
		setting.ModelRequestRateLimitEnabled = previousRateLimit
	})
	setting.ModelRequestRateLimitEnabled = false
	privacySetting.Enabled = true
	privacySetting.GitleaksTOML = filepath.Join(t.TempDir(), "missing-rules.toml")

	user := model.User{Username: "privacy-router-user", Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId: user.Id, Key: "privacyrouterkey", Status: common.TokenStatusEnabled,
		ExpiredTime: -1, UnlimitedQuota: true,
	}).Error)
	jsplugin.DefaultRegistry = jsplugin.NewRegistry()
	source, err := builtinplugins.Source("sunoapi")
	require.NoError(t, err)
	_, err = jsplugin.DefaultRegistry.RegisterFactory(source, jsplugin.Options{Key: "sunoapi"})
	require.NoError(t, err)

	engine := gin.New()
	SetRelayRouter(engine)
	SetTaskPluginProtocolRouter(engine)
	SetVideoRouter(engine)
	SetTaskRouter(engine)
	engine.NoRoute(SetPluginRouter(engine))

	// Register static and plugin routes together, as startup does. A static
	// /suno route must not displace the factory plugin or its protocol support.
	require.Empty(t, jsplugin.DefaultRegistry.RoutingErrors())
	_, available := jsplugin.DefaultRegistry.Get("sunoapi")
	require.True(t, available)

	for _, path := range []string{
		"/v1/chat/completions",
		"/v1/responses",
		"/v1/videos",
		"/v1/tasks/sunoapi",
		"/suno/submit/LYRICS",
		"/suno/fetch",
	} {
		t.Run(path, func(t *testing.T) {
			// Filter initialization must fail closed before model selection or
			// plugin decoding can cache and forward the unfiltered request.
			body := `{"model":"suno_lyrics","prompt":"contact review@example.com","input":"contact review@example.com","messages":[{"role":"user","content":"contact review@example.com"}]}`
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer sk-privacyrouterkey")
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			var response struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Equal(t, "privacy_filter_failed", response.Error.Code)

			request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			recorder = httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			assert.Equal(t, http.StatusUnauthorized, recorder.Code, recorder.Body.String())
			assert.NotContains(t, recorder.Body.String(), "privacy_filter_failed")
		})
	}

	// Native batch queries must still run through the Suno decoder/renderer,
	// both with filtering disabled and with the built-in rules enabled.
	for _, enabled := range []bool{false, true} {
		privacySetting.Enabled = enabled
		if enabled {
			privacySetting.GitleaksTOML = ""
		}
		request := httptest.NewRequest(http.MethodPost, "/suno/fetch", strings.NewReader(`{"ids":[],"prompt":"contact review@example.com"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer sk-privacyrouterkey")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		assert.JSONEq(t, `{"code":"success","message":"","data":[]}`, recorder.Body.String())
	}
}

func setupRelayRouterTestDB(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	originalIsMasterNode := common.IsMasterNode
	originalRedisEnabled := common.RedisEnabled
	originalSQLitePath := common.SQLitePath
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")

	common.IsMasterNode = false
	common.RedisEnabled = false
	common.SQLitePath = fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}))

	t.Cleanup(func() {
		if sqlDB, err := model.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		common.IsMasterNode = originalIsMasterNode
		common.RedisEnabled = originalRedisEnabled
		common.SQLitePath = originalSQLitePath
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", originalSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
	})
}
