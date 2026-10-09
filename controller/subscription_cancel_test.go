package controller

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelfSubscriptionLifecycleDatabaseMatrix(t *testing.T) {
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
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSubscription{}))
			model.LOG_DB, _ = newAuditTestDatabase(t, dialect.kind, os.Getenv(dialect.env))
			require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
			versionQuery := "SELECT version()"
			if dialect.kind == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s (separate log database)", version)

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
