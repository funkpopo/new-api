package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// SubscriptionPlan schema from the latest release, v1.0.0-rc.37.
type releasedVisibilityPlan struct {
	Id                      int
	Title                   string  `gorm:"type:varchar(128);not null"`
	Subtitle                string  `gorm:"type:varchar(255);default:''"`
	PriceAmount             float64 `gorm:"type:decimal(10,6);not null;default:0"`
	Currency                string  `gorm:"type:varchar(8);not null;default:'USD'"`
	DurationUnit            string  `gorm:"type:varchar(16);not null;default:'month'"`
	DurationValue           int     `gorm:"type:int;not null;default:1"`
	CustomSeconds           int64   `gorm:"type:bigint;not null;default:0"`
	Enabled                 bool    `gorm:"default:true"`
	SortOrder               int     `gorm:"type:int;default:0"`
	AllowBalancePay         *bool
	AllowWalletOverflow     *bool
	StripePriceId           string `gorm:"type:varchar(128);default:''"`
	CreemProductId          string `gorm:"type:varchar(128);default:''"`
	WaffoPancakeProductId   string `gorm:"type:varchar(128);default:''"`
	MaxPurchasePerUser      int    `gorm:"type:int;default:0"`
	UpgradeGroup            string `gorm:"type:varchar(64);default:''"`
	DowngradeGroup          string `gorm:"type:varchar(64);default:''"`
	TotalAmount             int64  `gorm:"type:bigint;not null;default:0"`
	QuotaResetPeriod        string `gorm:"type:varchar(16);default:'never'"`
	QuotaResetCustomSeconds int64  `gorm:"type:bigint;default:0"`
	CreatedAt               int64  `gorm:"bigint"`
	UpdatedAt               int64  `gorm:"bigint"`
}

func (releasedVisibilityPlan) TableName() string { return "subscription_plans" }

type subscriptionVisibilityResponse struct {
	Success bool
	Message string
	Data    []SubscriptionPlanDTO
}

func subscriptionVisibilityRequest(t *testing.T, handler gin.HandlerFunc, userID int, planID int, body any, output any) {
	t.Helper()
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(encoded))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", userID)
	// A stale/session group must never authorize a purchase.
	c.Set("group", "vip")
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(planID)}}
	handler(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), output), recorder.Body.String())
}

func TestSubscriptionVisibilityDatabaseMatrix(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	previousRatios := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":1,"partner":1}`))
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios)) })
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip("set " + dialect.env + " to run this database")
			}
			for _, upgrade := range []bool{false, true} {
				t.Run(map[bool]string{false: "fresh", true: "upgrade_rc37"}[upgrade], func(t *testing.T) {
					db, dsn := newAuditTestDatabase(t, dialect.kind, os.Getenv(dialect.env))
					legacy := releasedVisibilityPlan{Title: "Legacy plan", PriceAmount: 3.25, TotalAmount: 1234, AllowBalancePay: common.GetPointer(false)}
					if upgrade {
						require.NoError(t, db.AutoMigrate(&releasedVisibilityPlan{}))
						require.NoError(t, db.Create(&legacy).Error)
					}
					initialConnection, err := db.DB()
					require.NoError(t, err)
					require.NoError(t, initialConnection.Close())
					previousDB, previousLogDB := model.DB, model.LOG_DB
					previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
					previousMaster, previousPath, previousRedis := common.IsMasterNode, common.SQLitePath, common.RedisEnabled
					common.IsMasterNode, common.RedisEnabled = true, false
					if dialect.kind == "sqlite" {
						common.SQLitePath, dsn = dsn, "local"
					}
					t.Setenv("SQL_DSN", dsn)
					t.Setenv("LOG_SQL_DSN", "")
					t.Cleanup(func() {
						common.IsMasterNode, common.SQLitePath, common.RedisEnabled = previousMaster, previousPath, previousRedis
						common.SetDatabaseTypes(previousMain, previousLog)
						model.DB, model.LOG_DB = previousDB, previousLogDB
					})
					for range 2 {
						require.NoError(t, model.InitDB())
						db = model.DB
						connection, err := db.DB()
						require.NoError(t, err)
						require.NoError(t, connection.Close())
					}
					// Reopen without migrations for the behavioral checks.
					common.IsMasterNode = false
					require.NoError(t, model.InitDB())
					db = model.DB
					model.LOG_DB = db
					connection, err := db.DB()
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, connection.Close()) })
					query := "SELECT version()"
					if dialect.kind == "sqlite" {
						query = "SELECT sqlite_version()"
					}
					var version string
					require.NoError(t, db.Raw(query).Scan(&version).Error)
					t.Logf("database version: %s", version)
					if upgrade {
						var saved model.SubscriptionPlan
						require.NoError(t, db.First(&saved, legacy.Id).Error)
						assert.Equal(t, legacy.Title, saved.Title)
						assert.Equal(t, legacy.PriceAmount, saved.PriceAmount)
						assert.Equal(t, legacy.TotalAmount, saved.TotalAmount)
						require.NotNil(t, saved.AllowBalancePay)
						assert.False(t, *saved.AllowBalancePay)
						assert.Empty(t, saved.VisibleGroups)
						assert.True(t, saved.IsVisibleToGroup("any-new-group"))
						assert.Error(t, db.Create(&releasedVisibilityPlan{Id: legacy.Id, Title: "Duplicate"}).Error)
					}
					verifySubscriptionVisibility(t, db)
				})
			}
		})
	}
}

func verifySubscriptionVisibility(t *testing.T, db *gorm.DB) {
	t.Helper()
	user := model.User{Username: "visibility-user", Group: "default", Quota: 2000000}
	require.NoError(t, db.Create(&user).Error)
	public := model.SubscriptionPlan{Title: "Public", PriceAmount: 1, Enabled: true}
	var created struct {
		Success bool
		Message string
		Data    model.SubscriptionPlan
	}
	subscriptionVisibilityRequest(t, AdminCreateSubscriptionPlan, user.Id, 0, SubscriptionPlanDTO{Plan: public}, &created)
	require.True(t, created.Success, created.Message)
	public = created.Data
	model.InvalidateSubscriptionPlanCache(public.Id)
	t.Cleanup(func() { model.InvalidateSubscriptionPlanCache(public.Id) })
	_, err := model.GetSubscriptionPlanById(public.Id)
	require.NoError(t, err)
	// Restrict a previously cached public plan without touching that cache.
	require.NoError(t, db.Model(&public).Update("visible_groups", "vip,partner").Error)
	_, err = model.GetSubscriptionPlanForPurchase(user.Id, public.Id)
	require.EqualError(t, err, "该套餐不对当前用户分组开放")
	public.VisibleGroups = "vip,partner"

	var response subscriptionVisibilityResponse
	subscriptionVisibilityRequest(t, GetSubscriptionPlans, user.Id, 0, nil, &response)
	require.True(t, response.Success)
	for _, item := range response.Data {
		assert.NotEqual(t, public.Id, item.Plan.Id)
	}
	subscriptionVisibilityRequest(t, AdminListSubscriptionPlans, user.Id, 0, nil, &response)
	require.True(t, response.Success)
	require.NotEmpty(t, response.Data)
	assert.Equal(t, public.Id, response.Data[0].Plan.Id)
	assert.Equal(t, "vip,partner", response.Data[0].Plan.VisibleGroups)

	for _, handler := range []struct {
		name string
		fn   gin.HandlerFunc
	}{
		{"balance", SubscriptionRequestBalancePay}, {"epay", SubscriptionRequestEpay},
		{"stripe", SubscriptionRequestStripePay}, {"creem", SubscriptionRequestCreemPay},
		{"waffo", SubscriptionRequestWaffoPancakePay},
	} {
		t.Run("denied_"+handler.name, func(t *testing.T) {
			var result struct {
				Success bool
				Message string
			}
			subscriptionVisibilityRequest(t, handler.fn, user.Id, 0, map[string]any{"plan_id": public.Id, "payment_method": "alipay"}, &result)
			assert.False(t, result.Success)
			assert.Equal(t, "该套餐不对当前用户分组开放", result.Message)
		})
	}
	var orderCount, subscriptionCount int64
	require.NoError(t, db.Model(&model.SubscriptionOrder{}).Count(&orderCount).Error)
	require.NoError(t, db.Model(&model.UserSubscription{}).Count(&subscriptionCount).Error)
	assert.Zero(t, orderCount)
	assert.Zero(t, subscriptionCount)
	var savedUser model.User
	require.NoError(t, db.First(&savedUser, user.Id).Error)
	assert.Equal(t, user.Quota, savedUser.Quota)

	// A current database group, rather than session or cache, grants access.
	require.NoError(t, db.Model(&user).Update("group", "vip-extra").Error)
	_, err = model.GetSubscriptionPlanForPurchase(user.Id, public.Id)
	require.EqualError(t, err, "该套餐不对当前用户分组开放")
	require.NoError(t, db.Model(&user).Update("group", "partner").Error)
	_, err = model.GetSubscriptionPlanForPurchase(user.Id, public.Id)
	require.NoError(t, err)
	subscriptionVisibilityRequest(t, GetSubscriptionPlans, user.Id, 0, nil, &response)
	require.True(t, response.Success)
	require.NotEmpty(t, response.Data)
	assert.Equal(t, public.Id, response.Data[0].Plan.Id)
	require.NoError(t, model.PurchaseSubscriptionWithBalance(user.Id, public.Id))
	require.NoError(t, db.First(&savedUser, user.Id).Error)
	assert.Equal(t, user.Quota-int(common.QuotaPerUnit), savedUser.Quota)
	pendingOrder := model.SubscriptionOrder{UserId: user.Id, PlanId: public.Id, TradeNo: "visibility-pending-order", PaymentProvider: model.PaymentProviderEpay, Status: common.TopUpStatusPending}
	require.NoError(t, pendingOrder.Insert())
	// Group visibility changes never revoke purchased subscriptions or admin grants.
	require.NoError(t, db.Model(&user).Update("group", "default").Error)
	require.NoError(t, model.CompleteSubscriptionOrder(pendingOrder.TradeNo, "", model.PaymentProviderEpay, "alipay"))
	_, err = model.AdminBindSubscription(user.Id, public.Id, "test")
	require.NoError(t, err)
	subscriptions, err := model.GetAllActiveUserSubscriptions(user.Id)
	require.NoError(t, err)
	assert.Len(t, subscriptions, 3)

	for _, value := range []string{"missing", "vip,missing", ",", "vip,"} {
		public.VisibleGroups = value
		var result struct {
			Success bool
			Message string
		}
		subscriptionVisibilityRequest(t, AdminUpdateSubscriptionPlan, user.Id, public.Id, SubscriptionPlanDTO{Plan: public}, &result)
		assert.False(t, result.Success, value)
		subscriptionVisibilityRequest(t, AdminCreateSubscriptionPlan, user.Id, 0, SubscriptionPlanDTO{Plan: public}, &result)
		assert.False(t, result.Success, value)
	}
	public.VisibleGroups = " vip, partner,vip "
	var updated struct {
		Success bool
		Message string
	}
	subscriptionVisibilityRequest(t, AdminUpdateSubscriptionPlan, user.Id, public.Id, SubscriptionPlanDTO{Plan: public}, &updated)
	require.True(t, updated.Success, updated.Message)
	var savedPlan model.SubscriptionPlan
	require.NoError(t, db.First(&savedPlan, public.Id).Error)
	assert.Equal(t, "vip,partner", savedPlan.VisibleGroups)
	public.VisibleGroups = ""
	subscriptionVisibilityRequest(t, AdminUpdateSubscriptionPlan, user.Id, public.Id, SubscriptionPlanDTO{Plan: public}, &updated)
	require.True(t, updated.Success, updated.Message)
	_, err = model.GetSubscriptionPlanForPurchase(user.Id, public.Id)
	require.NoError(t, err)
	require.NoError(t, db.Model(&public).Update("enabled", false).Error)
	_, err = model.GetSubscriptionPlanForPurchase(user.Id, public.Id)
	require.EqualError(t, err, "套餐未启用")
	subscriptionVisibilityRequest(t, GetSubscriptionPlans, user.Id, 0, nil, &response)
	require.True(t, response.Success)
	for _, item := range response.Data {
		assert.NotEqual(t, public.Id, item.Plan.Id)
	}
}
