package controller

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSelfSubscriptionLifecycleDatabaseMatrix(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip("set " + dialect.env + " to run this database")
			}
			db, dsn := newAuditTestDatabase(t, dialect.kind, os.Getenv(dialect.env))
			connection, err := db.DB()
			require.NoError(t, err)
			require.NoError(t, connection.Close())
			previousDB, previousLogDB := model.DB, model.LOG_DB
			previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
			previousMaster, previousPath, previousRedis := common.IsMasterNode, common.SQLitePath, common.RedisEnabled
			common.IsMasterNode, common.RedisEnabled = false, false
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
			require.NoError(t, model.InitDB())
			db = model.DB
			connection, err = db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, connection.Close()) })
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSubscription{}, &model.SubscriptionPlan{}, &model.SubscriptionOrder{}, &model.SubscriptionPreConsumeRecord{}, &model.TopUp{}))
			model.LOG_DB, _ = newAuditTestDatabase(t, dialect.kind, os.Getenv(dialect.env))
			require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
			versionQuery := "SELECT version()"
			if dialect.kind == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s (separate log database)", version)

			plan := model.SubscriptionPlan{Title: "Lifecycle", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 1000}
			require.NoError(t, db.Create(&plan).Error)
			model.InvalidateSubscriptionPlanCache(plan.Id)
			t.Run("live quota and in-flight billing", testSubscriptionLiveQuota)
			t.Run("overlapping group entitlements", testSubscriptionOverlappingGroups)
			t.Run("restore across reset boundary", testSubscriptionRestoreReset)
			t.Run("issuance ignores stale plan cache", testSubscriptionFreshIssuance)
			t.Run("concurrent cancellation", testSubscriptionConcurrentCancellation)

			for index, tc := range []struct {
				name             string
				status           string
				endOffset        int64
				actor            string
				param            string
				downgrade        string
				sibling          bool
				wantOK           bool
				wantGroup        string
				resubscribe      bool
				adminInvalidated bool
				group            string
				noUpgrade        bool
			}{
				{name: "own active subscription", status: "active", endOffset: 3600, wantOK: true, wantGroup: "default"},
				{name: "explicit downgrade group", status: "active", endOffset: 3600, downgrade: "basic", wantOK: true, wantGroup: "basic"},
				{name: "another upgraded subscription remains", status: "active", endOffset: 3600, sibling: true, wantOK: true, wantGroup: "vip"},
				{name: "other user", status: "active", endOffset: 3600, actor: "other", wantGroup: "vip"},
				{name: "missing actor", status: "active", endOffset: 3600, actor: "missing", wantGroup: "vip"},
				{name: "expired by time", status: "active", endOffset: -1, wantGroup: "vip"},
				{name: "expired status", status: "expired", endOffset: 3600, wantGroup: "vip"},
				{name: "already cancelled", status: "cancelled", endOffset: 3600, wantGroup: "vip"},
				{name: "invalid id", status: "active", endOffset: 3600, param: "invalid", wantGroup: "vip"},
				{name: "nonexistent id", status: "active", endOffset: 3600, param: "2147483647", wantGroup: "vip"},
				{name: "restore own cancelled subscription", resubscribe: true, status: "cancelled", endOffset: 3600, group: "default", wantOK: true, wantGroup: "vip"},
				{name: "restore after group changed", resubscribe: true, status: "cancelled", endOffset: 3600, group: "basic", wantOK: true, wantGroup: "vip"},
				{name: "restore without group upgrade", resubscribe: true, status: "cancelled", endOffset: 3600, group: "basic", noUpgrade: true, wantOK: true, wantGroup: "basic"},
				{name: "restore with existing upgraded subscription", resubscribe: true, status: "cancelled", endOffset: 3600, sibling: true, wantOK: true, wantGroup: "vip"},
				{name: "restore other user's subscription", resubscribe: true, status: "cancelled", endOffset: 3600, actor: "other", wantGroup: "vip"},
				{name: "restore without actor", resubscribe: true, status: "cancelled", endOffset: 3600, actor: "missing", wantGroup: "vip"},
				{name: "restore expired cancellation", resubscribe: true, status: "cancelled", endOffset: -1, wantGroup: "vip"},
				{name: "restore at expiration boundary", resubscribe: true, status: "cancelled", endOffset: 0, wantGroup: "vip"},
				{name: "restore active subscription", resubscribe: true, status: "active", endOffset: 3600, wantGroup: "vip"},
				{name: "restore expired status", resubscribe: true, status: "expired", endOffset: 3600, wantGroup: "vip"},
				{name: "restore invalid id", resubscribe: true, status: "cancelled", endOffset: 3600, param: "invalid", wantGroup: "vip"},
				{name: "restore zero id", resubscribe: true, status: "cancelled", endOffset: 3600, param: "0", wantGroup: "vip"},
				{name: "restore nonexistent id", resubscribe: true, status: "cancelled", endOffset: 3600, param: "2147483647", wantGroup: "vip"},
				{name: "restore administrator invalidation", resubscribe: true, status: "active", endOffset: 3600, adminInvalidated: true, wantGroup: "default"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					now := common.GetTimestamp()
					user := model.User{Username: "cancel-" + strconv.Itoa(index), AffCode: "cancel" + strconv.Itoa(index), Group: "vip", Quota: 500, AuthVersion: 7}
					if tc.group != "" {
						user.Group = tc.group
					}
					require.NoError(t, db.Create(&user).Error)
					sub := model.UserSubscription{UserId: user.Id, PlanId: 1, Status: tc.status, StartTime: now - 60, EndTime: now + tc.endOffset, AmountTotal: 1000, AmountUsed: 250, UpgradeGroup: "vip", PrevUserGroup: "default", DowngradeGroup: tc.downgrade, LastResetTime: now - 60, NextResetTime: now + 1800}
					if tc.noUpgrade {
						sub.UpgradeGroup = ""
					}
					require.NoError(t, db.Create(&sub).Error)
					if tc.adminInvalidated {
						_, err := model.AdminInvalidateUserSubscription(sub.Id)
						require.NoError(t, err)
						require.NoError(t, db.First(&sub, sub.Id).Error)
						assert.LessOrEqual(t, sub.EndTime, common.GetTimestamp())
					}
					if tc.sibling {
						sibling := model.UserSubscription{UserId: user.Id, PlanId: 1, Status: "active", EndTime: now + 7200, UpgradeGroup: "vip"}
						require.NoError(t, db.Create(&sibling).Error)
					}
					actor := user.Id
					if tc.actor == "other" {
						actor += 10000
					} else if tc.actor == "missing" {
						actor = 0
					}
					param := tc.param
					if param == "" {
						param = strconv.Itoa(sub.Id)
					}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					action := "cancel"
					handler := CancelSelfSubscription
					if tc.resubscribe {
						action, handler = "resubscribe", ResubscribeSelfSubscription
					}
					c.Request = httptest.NewRequest(http.MethodPost, "/api/subscription/self/"+param+"/"+action, nil)
					c.Set("id", actor)
					c.Params = gin.Params{{Key: "id", Value: param}}
					handler(c)
					require.Equal(t, http.StatusOK, recorder.Code)
					var response struct{ Success bool }
					require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
					require.Equal(t, tc.wantOK, response.Success, recorder.Body.String())
					var saved model.UserSubscription
					require.NoError(t, db.First(&saved, sub.Id).Error)
					if tc.wantOK {
						if tc.resubscribe {
							assert.Equal(t, "active", saved.Status)
							require.Error(t, model.ResubscribeUserSubscription(user.Id, sub.Id), "repeated restoration must be rejected")
							if tc.group != "" && !tc.noUpgrade {
								assert.Equal(t, tc.group, saved.PrevUserGroup)
							}
						} else {
							assert.Equal(t, "cancelled", saved.Status)
							_, err := model.CancelUserSubscription(user.Id, sub.Id)
							require.Error(t, err, "repeated cancellation must not change the subscription")
						}
					} else {
						assert.Equal(t, sub, saved)
					}
					assert.Equal(t, sub.StartTime, saved.StartTime)
					assert.Equal(t, sub.EndTime, saved.EndTime)
					assert.Equal(t, sub.LastResetTime, saved.LastResetTime)
					assert.Equal(t, sub.NextResetTime, saved.NextResetTime)
					assert.Equal(t, sub.AmountUsed, saved.AmountUsed)
					assert.Equal(t, sub.AmountTotal, saved.AmountTotal)
					require.NoError(t, db.First(&user, user.Id).Error)
					assert.Equal(t, tc.wantGroup, user.Group)
					assert.Equal(t, 500, user.Quota, "cancellation does not refund wallet quota")
					assert.EqualValues(t, 7, user.AuthVersion)
					var logs []model.Log
					require.NoError(t, model.LOG_DB.Where("user_id = ?", user.Id).Find(&logs).Error)
					if tc.wantOK {
						require.Len(t, logs, 1)
						assert.Contains(t, logs[0].Content, strconv.Itoa(sub.Id))
					} else {
						assert.Empty(t, logs)
					}
					if tc.wantOK && !tc.resubscribe {
						require.NoError(t, model.ResubscribeUserSubscription(user.Id, sub.Id))
						require.NoError(t, db.First(&saved, sub.Id).Error)
						assert.Equal(t, "active", saved.Status)
						assert.Equal(t, sub.EndTime, saved.EndTime)
						assert.Equal(t, sub.AmountUsed, saved.AmountUsed)
						assert.Equal(t, sub.NextResetTime, saved.NextResetTime)
						require.NoError(t, db.First(&user, user.Id).Error)
						assert.Equal(t, "vip", user.Group)
						assert.Equal(t, 500, user.Quota)
						_, err := model.CancelUserSubscription(user.Id, sub.Id)
						require.NoError(t, err)
						require.NoError(t, db.First(&user, user.Id).Error)
						assert.Equal(t, tc.wantGroup, user.Group, "a second cancellation restores the correct group")
					}
				})
			}
		})
	}
}

func testSubscriptionLiveQuota(t *testing.T) {
	db := model.DB
	connection, err := db.DB()
	require.NoError(t, err)
	previousLimit := connection.Stats().MaxOpenConnections
	connection.SetMaxOpenConns(1)
	t.Cleanup(func() { connection.SetMaxOpenConns(previousLimit) })
	now := common.GetTimestamp()
	user := model.User{Username: "live-quota", AffCode: "live-quota", Group: "default", Quota: 500}
	require.NoError(t, db.Create(&user).Error)
	plan := model.SubscriptionPlan{Title: "Live quota", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 1000, AllowWalletOverflow: common.GetPointer(true)}
	require.NoError(t, db.Create(&plan).Error)
	active := model.UserSubscription{UserId: user.Id, PlanId: plan.Id, Status: "active", AmountTotal: 1000, AmountUsed: 500, StartTime: now - 60, EndTime: now + 7200, LastResetTime: now - 60, NextResetTime: now + 3600, AllowWalletOverflow: true}
	cancelled, expired := active, active
	cancelled.Status, expired.EndTime = "cancelled", now-1
	for _, sub := range []*model.UserSubscription{&active, &cancelled, &expired} {
		require.NoError(t, db.Create(sub).Error)
	}
	_, err = model.PreConsumeUserSubscription("live-refund", user.Id, "test", 0, 100)
	require.NoError(t, err)
	_, err = model.GetSubscriptionPlanById(plan.Id) // Prime the old plan cache.
	require.NoError(t, err)
	plan.TotalAmount, plan.AllowWalletOverflow = 200, common.GetPointer(false)
	var response struct{ Success bool }
	subscriptionVisibilityRequest(t, AdminUpdateSubscriptionPlan, user.Id, plan.Id, AdminUpsertSubscriptionPlanRequest{Plan: plan}, &response)
	require.True(t, response.Success)
	for _, original := range []model.UserSubscription{active, cancelled, expired} {
		var saved model.UserSubscription
		require.NoError(t, db.First(&saved, original.Id).Error)
		if original.Id == expired.Id {
			assert.EqualValues(t, 1000, saved.AmountTotal)
			assert.True(t, saved.AllowWalletOverflow)
		} else {
			assert.EqualValues(t, 200, saved.AmountTotal)
			assert.False(t, saved.AllowWalletOverflow)
		}
		assert.Equal(t, original.Status, saved.Status)
		assert.Equal(t, original.EndTime, saved.EndTime)
		assert.Equal(t, original.LastResetTime, saved.LastResetTime)
		assert.Equal(t, original.NextResetTime, saved.NextResetTime)
	}
	allow, err := model.UserActiveSubscriptionsAllowWalletOverflow(user.Id)
	require.NoError(t, err)
	assert.False(t, allow)
	_, err = model.PreConsumeUserSubscription("lowered-limit", user.Id, "test", 0, 1)
	require.Error(t, err)
	require.Error(t, model.ReserveUserSubscriptionQuota(active.Id, 1))
	require.NoError(t, model.PostConsumeUserSubscriptionDelta(active.Id, 25), "already performed work remains chargeable")

	// A failed refund-record write must roll back the quota change too.
	const callback = "test:subscription_refund_failure"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "subscription_pre_consume_records" {
			tx.AddError(errors.New("forced refund record failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Update().Remove(callback) })
	require.Error(t, model.RefundSubscriptionPreConsume("live-refund"))
	var saved model.UserSubscription
	require.NoError(t, db.First(&saved, active.Id).Error)
	assert.EqualValues(t, 625, saved.AmountUsed)
	require.NoError(t, db.Callback().Update().Remove(callback))
	require.NoError(t, model.RefundSubscriptionPreConsume("live-refund"))
	require.NoError(t, model.RefundSubscriptionPreConsume("live-refund"))
	require.NoError(t, db.First(&saved, active.Id).Error)
	assert.EqualValues(t, 525, saved.AmountUsed, "refund works above the limit and is idempotent")

	_, err = model.CancelUserSubscription(user.Id, active.Id)
	require.NoError(t, err)
	require.Error(t, model.ReserveUserSubscriptionQuota(active.Id, 1))
	require.NoError(t, model.PostConsumeUserSubscriptionDelta(active.Id, -25))
	require.NoError(t, model.ResubscribeUserSubscription(user.Id, active.Id))
	require.NoError(t, db.First(&saved, active.Id).Error)
	assert.EqualValues(t, 500, saved.AmountUsed)
	assert.EqualValues(t, 200, saved.AmountTotal)
	assert.Equal(t, active.EndTime, saved.EndTime)

	plan.TotalAmount, plan.AllowWalletOverflow = 800, common.GetPointer(true)
	require.NoError(t, model.UpdateSubscriptionPlan(&plan))
	reservation, err := model.PreConsumeUserSubscription("raised-limit", user.Id, "test", 0, 300)
	require.NoError(t, err)
	assert.EqualValues(t, 800, reservation.AmountTotal)
	assert.EqualValues(t, 800, reservation.AmountUsedAfter)
	plan.TotalAmount = 0
	require.NoError(t, model.UpdateSubscriptionPlan(&plan))
	require.NoError(t, model.ReserveUserSubscriptionQuota(active.Id, 100))
	allow, err = model.UserActiveSubscriptionsAllowWalletOverflow(user.Id)
	require.NoError(t, err)
	assert.True(t, allow)
	assert.Error(t, model.PostConsumeUserSubscriptionDelta(active.Id, math.MaxInt64))
	_, err = model.PreConsumeUserSubscription("overflow", user.Id, "test", 0, math.MaxInt64)
	require.Error(t, err)
	require.NoError(t, db.First(&saved, active.Id).Error)
	assert.EqualValues(t, 900, saved.AmountUsed)
	require.NoError(t, db.First(&user, user.Id).Error)
	assert.Equal(t, 500, user.Quota)
	missing := plan
	missing.Id = math.MaxInt32
	require.ErrorIs(t, model.UpdateSubscriptionPlan(&missing), gorm.ErrRecordNotFound)

	const planFailure = "test:subscription_plan_sync_failure"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(planFailure, func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "user_subscriptions" {
			tx.AddError(errors.New("forced subscription sync failure"))
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Update().Remove(planFailure) })
	plan.TotalAmount = 1500
	require.Error(t, model.UpdateSubscriptionPlan(&plan))
	var storedPlan model.SubscriptionPlan
	require.NoError(t, db.First(&storedPlan, plan.Id).Error)
	assert.Zero(t, storedPlan.TotalAmount, "plan and subscription limits roll back together")
	require.NoError(t, db.First(&saved, active.Id).Error)
	assert.Zero(t, saved.AmountTotal)
	require.NoError(t, db.Callback().Update().Remove(planFailure))
	plan.QuotaResetPeriod, plan.QuotaResetCustomSeconds = model.SubscriptionResetCustom, 3600
	require.NoError(t, model.UpdateSubscriptionPlan(&plan))
	require.NoError(t, db.First(&saved, active.Id).Error)
	assert.EqualValues(t, 900, saved.AmountUsed, "changing reset schedule does not grant free quota")
	assert.InDelta(t, common.GetTimestamp()+3600, saved.NextResetTime, 2)
	assert.Equal(t, active.EndTime, saved.EndTime)
	var paused model.UserSubscription
	require.NoError(t, db.First(&paused, cancelled.Id).Error)
	assert.Equal(t, saved.NextResetTime, paused.NextResetTime)
	assert.EqualValues(t, 500, paused.AmountUsed)
	plan.QuotaResetPeriod = model.SubscriptionResetNever
	require.NoError(t, model.UpdateSubscriptionPlan(&plan))
	require.NoError(t, db.First(&saved, active.Id).Error)
	assert.Zero(t, saved.NextResetTime)
	assert.EqualValues(t, 900, saved.AmountUsed)
}

func testSubscriptionOverlappingGroups(t *testing.T) {
	server := miniredis.RunT(t)
	previousRedis, previousClient := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = true, redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		_ = common.RDB.Close()
		common.RedisEnabled, common.RDB = previousRedis, previousClient
	})
	for index, tc := range []struct {
		name, secondGroup, action string
		olderFirst                bool
	}{
		{"same group older first", "vip", "cancel", true},
		{"same group newer first", "vip", "cancel", false},
		{"different groups older first", "pro", "cancel", true},
		{"legacy predecessor chain", "pro", "cancel", true},
		{"different groups newer first", "pro", "cancel", false},
		{"administrator deletion", "pro", "delete", false},
		{"expiry with another plan", "pro", "expire", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := model.User{Username: fmt.Sprintf("stack-%d", index), AffCode: fmt.Sprintf("stack-%d", index), Group: "default", AuthVersion: 7}
			require.NoError(t, model.DB.Create(&user).Error)
			_, err := model.GetUserCache(user.Id)
			require.NoError(t, err)
			var subs []model.UserSubscription
			for _, group := range []string{"vip", tc.secondGroup} {
				plan := model.SubscriptionPlan{Title: "Stack " + group, UpgradeGroup: group, DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 1000}
				require.NoError(t, model.DB.Create(&plan).Error)
				_, err := model.AdminBindSubscription(user.Id, plan.Id, "")
				require.NoError(t, err)
			}
			require.NoError(t, model.DB.Where("user_id = ?", user.Id).Order("id asc").Find(&subs).Error)
			require.Len(t, subs, 2)
			assert.Equal(t, "default", subs[0].PrevUserGroup)
			assert.Equal(t, "default", subs[1].PrevUserGroup)
			if tc.name == "legacy predecessor chain" {
				require.NoError(t, model.DB.Model(&subs[1]).Update("prev_user_group", "vip").Error)
			}
			first, remaining := subs[1], subs[0]
			if tc.olderFirst {
				first, remaining = subs[0], subs[1]
			}
			switch tc.action {
			case "delete":
				_, err := model.AdminDeleteUserSubscription(first.Id)
				require.NoError(t, err)
			case "expire":
				require.NoError(t, model.DB.Model(&first).Update("end_time", common.GetTimestamp()-1).Error)
				_, err := model.ExpireDueSubscriptions(100)
				require.NoError(t, err)
			default:
				_, err := model.CancelUserSubscription(user.Id, first.Id)
				require.NoError(t, err)
			}
			require.NoError(t, model.DB.First(&user, user.Id).Error)
			assert.Equal(t, remaining.UpgradeGroup, user.Group)
			cached, err := model.GetUserCache(user.Id)
			require.NoError(t, err)
			assert.Equal(t, remaining.UpgradeGroup, cached.Group)
			_, err = model.CancelUserSubscription(user.Id, remaining.Id)
			require.NoError(t, err)
			require.NoError(t, model.DB.First(&user, user.Id).Error)
			assert.Equal(t, "default", user.Group)
			cached, err = model.GetUserCache(user.Id)
			require.NoError(t, err)
			assert.Equal(t, "default", cached.Group)
			require.NoError(t, model.ResubscribeUserSubscription(user.Id, remaining.Id))
			require.NoError(t, model.DB.First(&user, user.Id).Error)
			assert.Equal(t, remaining.UpgradeGroup, user.Group)
			cached, err = model.GetUserCache(user.Id)
			require.NoError(t, err)
			assert.Equal(t, remaining.UpgradeGroup, cached.Group)
			_, err = model.CancelUserSubscription(user.Id, remaining.Id)
			require.NoError(t, err)
			require.NoError(t, model.DB.First(&user, user.Id).Error)
			assert.Equal(t, "default", user.Group)
			assert.EqualValues(t, 7, user.AuthVersion)
		})
	}

	t.Run("removing a cancelled subscription preserves a manual group change", func(t *testing.T) {
		user := model.User{Username: "cancelled-admin", AffCode: "cancelled-admin", Group: "vip"}
		require.NoError(t, model.DB.Create(&user).Error)
		sub := model.UserSubscription{UserId: user.Id, PlanId: 1, Status: "cancelled", UpgradeGroup: "vip", PrevUserGroup: "default", EndTime: common.GetTimestamp() + 3600}
		require.NoError(t, model.DB.Create(&sub).Error)
		_, err := model.AdminInvalidateUserSubscription(sub.Id)
		require.NoError(t, err)
		require.NoError(t, model.DB.First(&user, user.Id).Error)
		assert.Equal(t, "vip", user.Group)
		_, err = model.AdminDeleteUserSubscription(sub.Id)
		require.NoError(t, err)
		require.NoError(t, model.DB.First(&user, user.Id).Error)
		assert.Equal(t, "vip", user.Group)
	})
}

func testSubscriptionRestoreReset(t *testing.T) {
	user := model.User{Username: "restore-reset", AffCode: "restore-reset", Group: "default"}
	require.NoError(t, model.DB.Create(&user).Error)
	plan := model.SubscriptionPlan{Title: "Hourly", TotalAmount: 1000, QuotaResetPeriod: model.SubscriptionResetCustom, QuotaResetCustomSeconds: 3600}
	require.NoError(t, model.DB.Create(&plan).Error)
	now := common.GetTimestamp()
	sub := model.UserSubscription{UserId: user.Id, PlanId: plan.Id, Status: "cancelled", AmountTotal: 1000, AmountUsed: 600, StartTime: now - 7200, EndTime: now + 7200, LastResetTime: now - 7200, NextResetTime: now - 3600}
	require.NoError(t, model.DB.Create(&sub).Error)
	require.NoError(t, model.ResubscribeUserSubscription(user.Id, sub.Id))
	var saved model.UserSubscription
	require.NoError(t, model.DB.First(&saved, sub.Id).Error)
	assert.Zero(t, saved.AmountUsed)
	assert.Equal(t, sub.EndTime, saved.EndTime)
	assert.Greater(t, saved.NextResetTime, common.GetTimestamp())
	_, err := model.PreConsumeUserSubscription("after-restored-reset", user.Id, "test", 0, 900)
	require.NoError(t, err)
	_, err = model.CancelUserSubscription(user.Id, sub.Id)
	require.NoError(t, err)
	require.NoError(t, model.ResubscribeUserSubscription(user.Id, sub.Id))
	require.NoError(t, model.DB.First(&saved, sub.Id).Error)
	assert.EqualValues(t, 900, saved.AmountUsed, "restoring twice in the same period must not reset quota again")
}

func testSubscriptionFreshIssuance(t *testing.T) {
	connection, err := model.DB.DB()
	require.NoError(t, err)
	previousLimit := connection.Stats().MaxOpenConnections
	connection.SetMaxOpenConns(1)
	t.Cleanup(func() { connection.SetMaxOpenConns(previousLimit) })
	plan := model.SubscriptionPlan{Title: "Fresh issuance", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100}
	require.NoError(t, model.DB.Create(&plan).Error)
	model.InvalidateSubscriptionPlanCache(plan.Id)
	_, err = model.GetSubscriptionPlanById(plan.Id)
	require.NoError(t, err)
	// Simulate another process changing the plan while this process retains its cache.
	require.NoError(t, model.DB.Model(&plan).Update("total_amount", 900).Error)
	for _, source := range []string{"admin", "balance", "order"} {
		user := model.User{Username: "fresh-" + source, AffCode: "fresh-" + source, Group: "default"}
		require.NoError(t, model.DB.Create(&user).Error)
		switch source {
		case "admin":
			_, err = model.AdminBindSubscription(user.Id, plan.Id, "")
		case "balance":
			err = model.PurchaseSubscriptionWithBalance(user.Id, plan.Id)
		case "order":
			order := model.SubscriptionOrder{UserId: user.Id, PlanId: plan.Id, TradeNo: "fresh-order", Status: common.TopUpStatusPending}
			require.NoError(t, model.DB.Create(&order).Error)
			err = model.CompleteSubscriptionOrder(order.TradeNo, "", "", "")
		}
		require.NoError(t, err)
		var sub model.UserSubscription
		require.NoError(t, model.DB.Where("user_id = ?", user.Id).First(&sub).Error)
		assert.EqualValues(t, 900, sub.AmountTotal)
		_, err = model.PreConsumeUserSubscription("fresh-"+source, user.Id, "test", 0, 800)
		require.NoError(t, err)
	}
}

func testSubscriptionConcurrentCancellation(t *testing.T) {
	connection, err := model.DB.DB()
	require.NoError(t, err)
	previousLimit := connection.Stats().MaxOpenConnections
	connection.SetMaxOpenConns(4)
	// SQLite serializes writers; use its supported single-connection configuration.
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		connection.SetMaxOpenConns(1)
	}
	t.Cleanup(func() { connection.SetMaxOpenConns(previousLimit) })
	user := model.User{Username: "concurrent-cancel", AffCode: "concurrent-cancel", Group: "default"}
	require.NoError(t, model.DB.Create(&user).Error)
	plan := model.SubscriptionPlan{Title: "Concurrent", UpgradeGroup: "vip", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1}
	require.NoError(t, model.DB.Create(&plan).Error)
	for range 2 {
		_, err := model.AdminBindSubscription(user.Id, plan.Id, "")
		require.NoError(t, err)
	}
	var subs []model.UserSubscription
	require.NoError(t, model.DB.Where("user_id = ?", user.Id).Find(&subs).Error)
	require.Len(t, subs, 2)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, sub := range subs {
		go func() {
			<-start
			_, err := model.CancelUserSubscription(user.Id, sub.Id)
			results <- err
		}()
	}
	close(start)
	for range 2 {
		require.NoError(t, <-results)
	}
	require.NoError(t, model.DB.First(&user, user.Id).Error)
	assert.Equal(t, "default", user.Group)
	hasActive, err := model.HasActiveUserSubscription(user.Id)
	require.NoError(t, err)
	assert.False(t, hasActive)
}
