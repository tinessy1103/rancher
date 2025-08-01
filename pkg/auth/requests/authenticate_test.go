package requests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rancher/norman/types"
	ext "github.com/rancher/rancher/pkg/apis/ext.cattle.io/v1"
	apiv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/auth/accessor"
	"github.com/rancher/rancher/pkg/auth/providers"
	"github.com/rancher/rancher/pkg/auth/providers/common"
	"github.com/rancher/rancher/pkg/auth/tokens/hashers"
	"github.com/rancher/rancher/pkg/clusterrouter"
	exttokenstore "github.com/rancher/rancher/pkg/ext/stores/tokens"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	mgmtFakes "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3/fakes"
	"github.com/rancher/wrangler/v3/pkg/generic/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"
)

type fakeUserRefresher struct {
	called bool
	userID string
	force  bool
}

func (r *fakeUserRefresher) refreshUser(userID string, force bool) {
	r.called = true
	r.userID = userID
	r.force = force
}

func (r *fakeUserRefresher) reset() {
	r.called = false
	r.userID = ""
	r.force = false
}

type fakeProvider struct {
	name                       string
	disabled                   bool
	getUserExtraAttributesFunc func(v3.Principal) map[string][]string
}

func (p *fakeProvider) IsDisabledProvider() (bool, error) {
	return p.disabled, nil
}

func (p *fakeProvider) Logout(apiContext *types.APIContext, token accessor.TokenAccessor) error {
	panic("not implemented")
}

func (p *fakeProvider) LogoutAll(apiContext *types.APIContext, token accessor.TokenAccessor) error {
	panic("not implemented")
}

func (p *fakeProvider) GetName() string {
	return p.name
}

func (p *fakeProvider) AuthenticateUser(ctx context.Context, input interface{}) (v3.Principal, []v3.Principal, string, error) {
	panic("not implemented")
}

func (p *fakeProvider) SearchPrincipals(name, principalType string, myToken accessor.TokenAccessor) ([]v3.Principal, error) {
	panic("not implemented")
}

func (p *fakeProvider) GetPrincipal(principalID string, token accessor.TokenAccessor) (v3.Principal, error) {
	panic("not implemented")
}

func (p *fakeProvider) CustomizeSchema(schema *types.Schema) {
	panic("not implemented")
}

func (p *fakeProvider) TransformToAuthProvider(authConfig map[string]interface{}) (map[string]interface{}, error) {
	panic("not implemented")
}

func (p *fakeProvider) RefetchGroupPrincipals(principalID string, secret string) ([]v3.Principal, error) {
	panic("not implemented")
}

func (p *fakeProvider) CanAccessWithGroupProviders(userPrincipalID string, groups []v3.Principal) (bool, error) {
	panic("not implemented")
}

func (p *fakeProvider) GetUserExtraAttributes(userPrincipal v3.Principal) map[string][]string {
	if p.getUserExtraAttributesFunc != nil {
		return p.getUserExtraAttributesFunc(userPrincipal)
	}

	return map[string][]string{
		common.UserAttributePrincipalID: {userPrincipal.Name},
		common.UserAttributeUserName:    {userPrincipal.LoginName},
	}
}

func (p *fakeProvider) CleanupResources(*v3.AuthConfig) error {
	return nil
}

func TestTokenAuthenticatorAuthenticate(t *testing.T) {
	existingProviders := providers.Providers
	defer func() {
		providers.Providers = existingProviders
	}()

	fakeProvider := &fakeProvider{
		name: "fake",
	}
	providers.Providers = map[string]common.AuthProvider{
		fakeProvider.name: fakeProvider,
	}

	now := time.Now()
	userID := "u-abcdef"
	userPrincipalID := fakeProvider.name + "_user://12345"

	user := &v3.User{
		ObjectMeta: metav1.ObjectMeta{
			Name: userID,
		},
		Username:     "fake-user",
		PrincipalIDs: []string{userPrincipalID},
	}

	token := &v3.Token{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "token-v2rcx",
			CreationTimestamp: metav1.NewTime(now),
		},
		Token:        "jnb9tksmnctvgbn92ngbkptblcjwg4pmfp98wqj29wk5kv85ktg59s",
		AuthProvider: fakeProvider.name,
		TTLMillis:    57600000,
		UserID:       userID,
		UserPrincipal: v3.Principal{
			ObjectMeta: metav1.ObjectMeta{
				Name: userPrincipalID,
			},
			Me:            true,
			PrincipalType: "user",
			Provider:      fakeProvider.name,
			LoginName:     user.Username,
		},
	}

	mockIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	mockIndexer.AddIndexers(cache.Indexers{tokenKeyIndex: tokenKeyIndexer})
	mockIndexer.Add(token)

	var patchData []byte
	ctrl := gomock.NewController(t)
	tokenClient := fake.NewMockNonNamespacedClientInterface[*apiv3.Token, *apiv3.TokenList](ctrl)
	tokenClient.EXPECT().Get(token.Name, metav1.GetOptions{}).Return(token, nil).AnyTimes()
	tokenClient.EXPECT().Patch(token.Name, k8stypes.JSONPatchType, gomock.Any()).DoAndReturn(func(name string, pt k8stypes.PatchType, data []byte, subresources ...any) (*apiv3.Token, error) {
		patchData = data
		return nil, nil
	}).AnyTimes()

	userAttribute := &v3.UserAttribute{
		ObjectMeta: metav1.ObjectMeta{
			Name: userID,
		},
		GroupPrincipals: map[string]apiv3.Principals{
			fakeProvider.name: {
				Items: []apiv3.Principal{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: fakeProvider.name + "_group://56789",
						},
						MemberOf:      true,
						LoginName:     "rancher",
						DisplayName:   "rancher",
						PrincipalType: "group",
						Provider:      fakeProvider.name,
					},
				},
			},
		},
		ExtraByProvider: map[string]map[string][]string{
			fakeProvider.name: {
				common.UserAttributePrincipalID: {userPrincipalID},
				common.UserAttributeUserName:    {user.Username},
			},
			providers.LocalProvider: {
				common.UserAttributePrincipalID: {"local://" + userID},
				common.UserAttributeUserName:    {"local-user"},
			},
		},
	}
	userAttributeLister := &mgmtFakes.UserAttributeListerMock{
		GetFunc: func(namespace, name string) (*v3.UserAttribute, error) {
			return userAttribute, nil
		},
	}

	userLister := &mgmtFakes.UserListerMock{
		GetFunc: func(namespace, name string) (*v3.User, error) {
			return user, nil
		},
	}

	userRefresher := &fakeUserRefresher{}

	authenticator := tokenAuthenticator{
		ctx:                 context.Background(),
		tokenIndexer:        mockIndexer,
		tokenClient:         tokenClient,
		userAttributeLister: userAttributeLister,
		userLister:          userLister,
		clusterRouter:       clusterrouter.GetClusterID,
		refreshUser:         userRefresher.refreshUser,
		now: func() time.Time {
			return now
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces", nil)
	req.Header.Set("Authorization", "Bearer "+token.Name+":"+token.Token)

	t.Run("authenticate", func(t *testing.T) {
		patchData = nil
		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.Equal(t, userID, resp.User)
		assert.Equal(t, userPrincipalID, resp.UserPrincipal)
		assert.Contains(t, resp.Groups, fakeProvider.name+"_group://56789")
		assert.Contains(t, resp.Groups, "system:cattle:authenticated")
		assert.Contains(t, resp.Extras[common.UserAttributePrincipalID], userPrincipalID)
		assert.Contains(t, resp.Extras[common.UserAttributeUserName], "fake-user")
		assert.True(t, userRefresher.called)
		assert.Equal(t, userID, userRefresher.userID)
		assert.False(t, userRefresher.force)
		require.NotEmpty(t, patchData)
		require.Len(t, resp.Extras[common.ExtraRequestTokenID], 1)
		assert.Equal(t, token.Name, resp.Extras[common.ExtraRequestTokenID][0])
		require.Len(t, resp.Extras[common.ExtraRequestHost], 1)
		require.Equal(t, req.Host, resp.Extras[common.ExtraRequestHost][0])
	})

	t.Run("subsecond lastUsedAt updates are throttled", func(t *testing.T) {
		oldTokenLastUsedAt := token.LastUsedAt
		defer func() {
			token.LastUsedAt = oldTokenLastUsedAt
		}()
		lastUsedAt := metav1.NewTime(now.Truncate(time.Second))
		token.LastUsedAt = &lastUsedAt
		patchData = nil
		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Empty(t, patchData)
	})

	t.Run("past and future lastUsedAt are updated", func(t *testing.T) {
		oldTokenLastUsedAt := token.LastUsedAt
		defer func() {
			token.LastUsedAt = oldTokenLastUsedAt
		}()
		lastUsedAt := metav1.NewTime(now.Add(-time.Second).Truncate(time.Second))
		token.LastUsedAt = &lastUsedAt
		patchData = nil
		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.NotEmpty(t, patchData)

		lastUsedAt = metav1.NewTime(now.Add(time.Second).Truncate(time.Second))
		token.LastUsedAt = &lastUsedAt
		patchData = nil

		resp, err = authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.NotEmpty(t, patchData)
	})

	t.Run("error updating lastUsedAt doesn't fail the request", func(t *testing.T) {
		oldTokenLastUsedAt := token.LastUsedAt
		defer func() {
			token.LastUsedAt = oldTokenLastUsedAt
			authenticator.tokenClient = tokenClient
		}()
		client := fake.NewMockNonNamespacedClientInterface[*apiv3.Token, *apiv3.TokenList](ctrl)
		client.EXPECT().Patch(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil, fmt.Errorf("some error")).Times(1)
		authenticator.tokenClient = client

		lastUsedAt := metav1.NewTime(now.Add(-time.Second).Truncate(time.Second))
		token.LastUsedAt = &lastUsedAt
		patchData = nil
		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Empty(t, patchData)
	})

	t.Run("token fetched with token client", func(t *testing.T) {
		defer mockIndexer.Add(token)
		mockIndexer.Delete(token)

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
	})

	t.Run("authenticate with a cluster specific token", func(t *testing.T) {
		clusterID := "c-955nj"
		oldTokenClusterName := token.ClusterName
		defer func() { token.ClusterName = oldTokenClusterName }()
		token.ClusterName = clusterID

		clusterReq := httptest.NewRequest(http.MethodGet, "/k8s/clusters/"+clusterID+"/v1/management.cattle.io.authconfigs", nil)
		clusterReq.Header.Set("Authorization", "Bearer "+token.Name+":"+token.Token)

		userRefresher.reset()

		resp, err := authenticator.Authenticate(clusterReq)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
	})

	t.Run("authenticate if userattribute doesn't exist", func(t *testing.T) {
		oldGetUserAttributeFunc := userAttributeLister.GetFunc
		defer func() { userAttributeLister.GetFunc = oldGetUserAttributeFunc }()
		userAttributeLister.GetFunc = func(namespace, name string) (*v3.UserAttribute, error) {
			return nil, apierrors.NewNotFound(schema.GroupResource{}, name)
		}

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
	})

	t.Run("retrieve extra from the provider if missing in userattribute", func(t *testing.T) {
		oldUserAttributeExtra := userAttribute.ExtraByProvider
		defer func() {
			userAttribute.ExtraByProvider = oldUserAttributeExtra
		}()
		userAttribute.ExtraByProvider = nil

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
		assert.Contains(t, resp.Extras[common.UserAttributePrincipalID], user.PrincipalIDs[0])
		assert.Contains(t, resp.Extras[common.UserAttributeUserName], user.Username)
	})

	t.Run("fill extra from the user if unable to get from neither userattribute nor provider", func(t *testing.T) {
		oldUserAttributeExtra := userAttribute.ExtraByProvider
		oldFakeProviderGetUserExtraAttributesFunc := fakeProvider.getUserExtraAttributesFunc
		defer func() {
			userAttribute.ExtraByProvider = oldUserAttributeExtra
			fakeProvider.getUserExtraAttributesFunc = oldFakeProviderGetUserExtraAttributesFunc
		}()
		userAttribute.ExtraByProvider = nil
		fakeProvider.getUserExtraAttributesFunc = func(userPrincipal v3.Principal) map[string][]string { return map[string][]string{} }

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)

		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
		assert.Contains(t, resp.Extras[common.UserAttributePrincipalID], user.PrincipalIDs[0])
		assert.Contains(t, resp.Extras[common.UserAttributeUserName], user.Username)
	})

	t.Run("provider refresh is not called if token user id has system prefix", func(t *testing.T) {
		oldTokenUserID := token.UserID
		defer func() { token.UserID = oldTokenUserID }()
		token.UserID = "system://provisioning/fleet-local/local"

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.False(t, userRefresher.called)
	})

	t.Run("provider refresh is not called for system users", func(t *testing.T) {
		oldGetUserFunc := userLister.GetFunc
		defer func() { userLister.GetFunc = oldGetUserFunc }()
		userLister.GetFunc = func(namespace, name string) (*v3.User, error) {
			return &v3.User{
				ObjectMeta: metav1.ObjectMeta{
					Name: userID,
				},
				PrincipalIDs: []string{
					"system://provisioning/fleet-local/local",
					"local://" + userID,
				},
			}, nil
		}

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.False(t, userRefresher.called)
	})

	t.Run("don't check provider if not specified in the token", func(t *testing.T) {
		oldTokenAuthProvider := token.AuthProvider
		defer func() { token.AuthProvider = oldTokenAuthProvider }()
		token.AuthProvider = ""

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
	})

	t.Run("token not found", func(t *testing.T) {
		defer func() {
			mockIndexer.Add(token)
			authenticator.tokenClient = tokenClient
		}()
		mockIndexer.Delete(token)
		client := fake.NewMockNonNamespacedClientInterface[*apiv3.Token, *apiv3.TokenList](ctrl)
		client.EXPECT().Get(token.Name, metav1.GetOptions{}).Return(nil, apierrors.NewNotFound(schema.GroupResource{}, token.Name)).Times(1)
		authenticator.tokenClient = client

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("failed to retrieve auth token with the token client", func(t *testing.T) {
		defer func() {
			mockIndexer.Add(token)
			authenticator.tokenClient = tokenClient
		}()
		mockIndexer.Delete(token)
		client := fake.NewMockNonNamespacedClientInterface[*apiv3.Token, *apiv3.TokenList](ctrl)
		client.EXPECT().Get(token.Name, metav1.GetOptions{}).Return(nil, fmt.Errorf("some error")).Times(1)
		authenticator.tokenClient = client

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("token is disabled", func(t *testing.T) {
		oldTokenEnabled := token.Enabled
		defer func() { token.Enabled = oldTokenEnabled }()
		token.Enabled = pointer.BoolPtr(false)

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("cluster ID doesn't match", func(t *testing.T) {
		clusterID := "c-955nj"
		oldTokenClusterName := token.ClusterName
		defer func() { token.ClusterName = oldTokenClusterName }()
		token.ClusterName = clusterID

		clusterReq := httptest.NewRequest(http.MethodGet, "/k8s/clusters/c-unknown/v1/management.cattle.io.authconfigs", nil)
		clusterReq.Header.Set("Authorization", "Bearer "+token.Name+":"+token.Token)

		userRefresher.reset()

		resp, err := authenticator.Authenticate(clusterReq)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
	})

	t.Run("user doesn't exist", func(t *testing.T) {
		oldGetUserFunc := userLister.GetFunc
		defer func() { userLister.GetFunc = oldGetUserFunc }()
		userLister.GetFunc = func(namespace, name string) (*v3.User, error) {
			return nil, apierrors.NewNotFound(schema.GroupResource{}, name)
		}

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("user is disabled", func(t *testing.T) {
		oldGetUserFunc := userLister.GetFunc
		defer func() { userLister.GetFunc = oldGetUserFunc }()
		userLister.GetFunc = func(namespace, name string) (*v3.User, error) {
			return &v3.User{
				ObjectMeta: metav1.ObjectMeta{
					Name: userID,
				},
				Enabled: pointer.BoolPtr(false),
			}, nil
		}

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("error getting userattribute", func(t *testing.T) {
		oldGetUserAttributeFunc := userAttributeLister.GetFunc
		defer func() { userAttributeLister.GetFunc = oldGetUserAttributeFunc }()
		userAttributeLister.GetFunc = func(namespace, name string) (*v3.UserAttribute, error) {
			return nil, fmt.Errorf("some error")
		}

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("auth provider is disabled", func(t *testing.T) {
		oldIsDisabled := fakeProvider.disabled
		defer func() { fakeProvider.disabled = oldIsDisabled }()
		fakeProvider.disabled = true

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("auth provider doesn't exist", func(t *testing.T) {
		oldProvider := token.AuthProvider
		defer func() { token.AuthProvider = oldProvider }()
		token.AuthProvider = "foo"

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})
	t.Run("failed to verify token: token expired", func(t *testing.T) {
		oldTokenCreationTimestamp := token.CreationTimestamp
		defer func() { token.CreationTimestamp = oldTokenCreationTimestamp }()
		token.CreationTimestamp = metav1.NewTime(now.Add(-time.Duration(token.TTLMillis)*time.Millisecond - 1))

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})
	t.Run("failed to verify token: mismatched", func(t *testing.T) {
		userRefresher.reset()

		mismatchedToken := "5cncldxmczdzsqtj7kwxqldjf6dhnn5vhr42vqd6mt878wrvwnrwc8"

		req := httptest.NewRequest(http.MethodGet, "/v1/namespaces", nil)
		req.Header.Set("Authorization", "Bearer "+token.Name+":"+mismatchedToken)

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})
}

func TestTokenAuthenticatorAuthenticateExtToken(t *testing.T) {
	existingProviders := providers.Providers
	defer func() {
		providers.Providers = existingProviders
	}()

	fakeProvider := &fakeProvider{
		name: "fake",
	}
	providers.Providers = map[string]common.AuthProvider{
		fakeProvider.name: fakeProvider,
	}

	now := time.Now()
	userID := "u-abcdef"
	userPrincipalID := fakeProvider.name + "_user://12345"

	user := &v3.User{
		ObjectMeta: metav1.ObjectMeta{
			Name: userID,
		},
		Username:     "fake-user",
		PrincipalIDs: []string{userPrincipalID},
	}

	tokenValue := "jnb9tksmnctvgbn92ngbkptblcjwg4pmfp98wqj29wk5kv85ktg59s"
	// note: ext tokens do not store their token value/secret
	tokenHash, _ := hashers.GetHasher().CreateHash(tokenValue)
	token := &ext.Token{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "token-v2rcx",
			CreationTimestamp: metav1.NewTime(now),
			Labels: map[string]string{
				exttokenstore.UserIDLabel: userID,
			},
		},
		Spec: ext.TokenSpec{
			UserID:  userID,
			TTL:     57600000,
			Kind:    exttokenstore.IsLogin,
			Enabled: pointer.Bool(true),
			UserPrincipal: ext.TokenPrincipal{
				Name:        userPrincipalID,
				Provider:    fakeProvider.name,
				LoginName:   user.Username,
				DisplayName: user.DisplayName,
			},
		},
		Status: ext.TokenStatus{
			Hash:           tokenHash,
			LastUpdateTime: "13:00",
		},
	}
	principalBytes, _ := json.Marshal(token.Spec.UserPrincipal)
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "token-v2rcx",
			CreationTimestamp: metav1.NewTime(now),
			Labels: map[string]string{
				exttokenstore.UserIDLabel:     userID,
				exttokenstore.SecretKindLabel: exttokenstore.SecretKindLabelValue,
			},
		},
		Data: map[string][]byte{
			exttokenstore.FieldEnabled:        []byte("true"),
			exttokenstore.FieldHash:           []byte(tokenHash),
			exttokenstore.FieldKind:           []byte(exttokenstore.IsLogin),
			exttokenstore.FieldLastUpdateTime: []byte("13:00"),
			exttokenstore.FieldPrincipal:      principalBytes,
			exttokenstore.FieldTTL:            []byte("57600000"),
			exttokenstore.FieldUID:            []byte("2905498-kafld-lkad"),
			exttokenstore.FieldUserID:         []byte(userID),
		},
	}

	var patchData []byte
	ctrl := gomock.NewController(t)

	userAttribute := &v3.UserAttribute{
		ObjectMeta: metav1.ObjectMeta{
			Name: userID,
		},
		GroupPrincipals: map[string]apiv3.Principals{
			fakeProvider.name: {
				Items: []apiv3.Principal{
					{
						ObjectMeta: metav1.ObjectMeta{
							Name: fakeProvider.name + "_group://56789",
						},
						MemberOf:      true,
						LoginName:     "rancher",
						DisplayName:   "rancher",
						PrincipalType: "group",
						Provider:      fakeProvider.name,
					},
				},
			},
		},
		ExtraByProvider: map[string]map[string][]string{
			fakeProvider.name: {
				common.UserAttributePrincipalID: {userPrincipalID},
				common.UserAttributeUserName:    {user.Username},
			},
			providers.LocalProvider: {
				common.UserAttributePrincipalID: {"local://" + userID},
				common.UserAttributeUserName:    {"local-user"},
			},
		},
	}
	userAttributeLister := &mgmtFakes.UserAttributeListerMock{
		GetFunc: func(namespace, name string) (*v3.UserAttribute, error) {
			return userAttribute, nil
		},
	}

	userLister := &mgmtFakes.UserListerMock{
		GetFunc: func(namespace, name string) (*v3.User, error) {
			return user, nil
		},
	}

	userRefresher := &fakeUserRefresher{}

	secrets := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
	scache := fake.NewMockCacheInterface[*corev1.Secret](ctrl)
	users := fake.NewMockNonNamespacedControllerInterface[*apiv3.User, *apiv3.UserList](ctrl)

	users.EXPECT().Cache().Return(nil).AnyTimes()
	secrets.EXPECT().Cache().Return(scache)

	scache.EXPECT().
		Get("cattle-tokens", token.Name).
		Return(tokenSecret, nil).
		AnyTimes()
	secrets.EXPECT().Patch("cattle-tokens", token.Name, k8stypes.JSONPatchType, gomock.Any()).
		DoAndReturn(func(space, name string, pt k8stypes.PatchType, data []byte, subresources ...any) (*apiv3.Token, error) {
			patchData = data
			return nil, nil
		}).AnyTimes()

	store := exttokenstore.NewSystem(nil, nil, secrets, users, nil, nil, nil, nil)

	authenticator := tokenAuthenticator{
		ctx:                 context.Background(),
		userAttributeLister: userAttributeLister,
		userLister:          userLister,
		clusterRouter:       clusterrouter.GetClusterID,
		refreshUser:         userRefresher.refreshUser,
		now: func() time.Time {
			return now
		},
		extTokenStore: store,
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/namespaces", nil)
	req.Header.Set("Authorization", "Bearer ext/"+token.Name+":"+tokenValue)

	t.Run("authenticate", func(t *testing.T) {
		patchData = nil
		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.Equal(t, userID, resp.User)
		assert.Equal(t, userPrincipalID, resp.UserPrincipal)
		assert.Contains(t, resp.Groups, fakeProvider.name+"_group://56789")
		assert.Contains(t, resp.Groups, "system:cattle:authenticated")
		assert.Contains(t, resp.Extras[common.UserAttributePrincipalID], userPrincipalID)
		assert.Contains(t, resp.Extras[common.UserAttributeUserName], "fake-user")
		assert.True(t, userRefresher.called)
		assert.Equal(t, userID, userRefresher.userID)
		assert.False(t, userRefresher.force)
		require.NotEmpty(t, patchData)
	})

	t.Run("subsecond lastUsedAt updates are throttled", func(t *testing.T) {
		oldTokenLastUsedAt := tokenSecret.Data["last-used-at"]
		defer func() {
			tokenSecret.Data["last-used-at"] = oldTokenLastUsedAt
		}()
		tokenSecret.Data["last-used-at"] = []byte(now.
			Truncate(time.Second).
			Format(time.RFC3339))
		patchData = nil
		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Empty(t, patchData)
	})

	t.Run("past and future lastUsedAt are updated", func(t *testing.T) {
		oldTokenLastUsedAt := tokenSecret.Data["last-used-at"]
		defer func() {
			tokenSecret.Data["last-used-at"] = oldTokenLastUsedAt
		}()
		tokenSecret.Data["last-used-at"] = []byte(now.
			Add(-time.Second).
			Truncate(time.Second).
			Format(time.RFC3339))
		patchData = nil
		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.NotEmpty(t, patchData)

		tokenSecret.Data["last-used-at"] = []byte(now.
			Add(time.Second).
			Truncate(time.Second).
			Format(time.RFC3339))
		patchData = nil

		resp, err = authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.NotEmpty(t, patchData)
	})

	t.Run("error updating lastUsedAt doesn't fail the request", func(t *testing.T) {
		oldTokenLastUsedAt := tokenSecret.Data["last-used-at"]
		defer func() {
			tokenSecret.Data["last-used-at"] = oldTokenLastUsedAt
			authenticator.extTokenStore = store
		}()
		newSecrets := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
		newSCache := fake.NewMockCacheInterface[*corev1.Secret](ctrl)
		newSecrets.EXPECT().Cache().Return(newSCache)

		newSCache.EXPECT().
			Get("cattle-tokens", token.Name).
			Return(tokenSecret, nil).
			AnyTimes()
		newSecrets.EXPECT().Patch("cattle-tokens", token.Name, k8stypes.JSONPatchType, gomock.Any()).
			Return(nil, fmt.Errorf("some error")).Times(1)
		authenticator.extTokenStore = exttokenstore.NewSystem(nil, nil, newSecrets, users, nil, nil, nil, nil)

		tokenSecret.Data["last-used-at"] = []byte(now.
			Add(-time.Second).
			Truncate(time.Second).
			Format(time.RFC3339))
		patchData = nil
		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		require.Empty(t, patchData)
	})

	t.Run("token fetched with token client", func(t *testing.T) {
		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
	})

	t.Run("authenticate if userattribute doesn't exist", func(t *testing.T) {
		oldGetUserAttributeFunc := userAttributeLister.GetFunc
		defer func() { userAttributeLister.GetFunc = oldGetUserAttributeFunc }()
		userAttributeLister.GetFunc = func(namespace, name string) (*v3.UserAttribute, error) {
			return nil, apierrors.NewNotFound(schema.GroupResource{}, name)
		}

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
	})

	t.Run("retrieve extra from the provider if missing in userattribute", func(t *testing.T) {
		oldUserAttributeExtra := userAttribute.ExtraByProvider
		defer func() {
			userAttribute.ExtraByProvider = oldUserAttributeExtra
		}()
		userAttribute.ExtraByProvider = nil

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)

		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
		assert.Contains(t, resp.Extras[common.UserAttributePrincipalID], user.PrincipalIDs[0])
		assert.Contains(t, resp.Extras[common.UserAttributeUserName], user.Username)
	})

	t.Run("fill extra from the user if unable to get from neither userattribute nor provider", func(t *testing.T) {
		oldUserAttributeExtra := userAttribute.ExtraByProvider
		oldFakeProviderGetUserExtraAttributesFunc := fakeProvider.getUserExtraAttributesFunc
		defer func() {
			userAttribute.ExtraByProvider = oldUserAttributeExtra
			fakeProvider.getUserExtraAttributesFunc = oldFakeProviderGetUserExtraAttributesFunc
		}()
		userAttribute.ExtraByProvider = nil
		fakeProvider.getUserExtraAttributesFunc = func(userPrincipal v3.Principal) map[string][]string { return map[string][]string{} }

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
		assert.Contains(t, resp.Extras[common.UserAttributePrincipalID], user.PrincipalIDs[0])
		assert.Contains(t, resp.Extras[common.UserAttributeUserName], user.Username)
	})

	t.Run("provider refresh is not called if token user id has system prefix", func(t *testing.T) {
		oldTokenUserID := tokenSecret.Data["user-id"]
		defer func() {
			tokenSecret.Data["user-id"] = oldTokenUserID
		}()
		tokenSecret.Data["user-id"] = []byte("system://provisioning/fleet-local/local")

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.False(t, userRefresher.called)
	})

	t.Run("provider refresh is not called for system users", func(t *testing.T) {
		oldGetUserFunc := userLister.GetFunc
		defer func() { userLister.GetFunc = oldGetUserFunc }()
		userLister.GetFunc = func(namespace, name string) (*v3.User, error) {
			return &v3.User{
				ObjectMeta: metav1.ObjectMeta{
					Name: userID,
				},
				PrincipalIDs: []string{
					"system://provisioning/fleet-local/local",
					"local://" + userID,
				},
			}, nil
		}

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.False(t, userRefresher.called)
	})

	t.Run("don't check provider if not specified in the token", func(t *testing.T) {
		oldTokenAuthProvider := token.Spec.UserPrincipal.Provider
		defer func() { token.Spec.UserPrincipal.Provider = oldTokenAuthProvider }()
		token.Spec.UserPrincipal.Provider = ""

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.True(t, resp.IsAuthed)
		assert.True(t, userRefresher.called)
	})

	t.Run("token not found", func(t *testing.T) {
		defer func() {
			authenticator.extTokenStore = store
		}()
		newSecrets := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
		newSCache := fake.NewMockCacheInterface[*corev1.Secret](ctrl)
		newSecrets.EXPECT().Cache().Return(newSCache)

		newSCache.EXPECT().
			Get("cattle-tokens", token.Name).
			Return(nil, apierrors.NewNotFound(schema.GroupResource{}, token.Name)).
			Times(1)
		authenticator.extTokenStore = exttokenstore.NewSystem(nil, nil, newSecrets, users, nil, nil, nil, nil)

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("failed to retrieve auth token with the token client", func(t *testing.T) {
		defer func() {
			authenticator.extTokenStore = store
		}()
		newSecrets := fake.NewMockControllerInterface[*corev1.Secret, *corev1.SecretList](ctrl)
		newSCache := fake.NewMockCacheInterface[*corev1.Secret](ctrl)
		newSecrets.EXPECT().Cache().Return(newSCache)

		newSCache.EXPECT().
			Get("cattle-tokens", token.Name).
			Return(nil, fmt.Errorf("some error")).
			Times(1)
		authenticator.extTokenStore = exttokenstore.NewSystem(nil, nil, newSecrets, users, nil, nil, nil, nil)

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("token is disabled", func(t *testing.T) {
		oldTokenEnabled := tokenSecret.Data["enabled"]
		defer func() {
			tokenSecret.Data["enabled"] = oldTokenEnabled
		}()
		tokenSecret.Data["enabled"] = []byte("false")

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("user doesn't exist", func(t *testing.T) {
		oldGetUserFunc := userLister.GetFunc
		defer func() { userLister.GetFunc = oldGetUserFunc }()
		userLister.GetFunc = func(namespace, name string) (*v3.User, error) {
			return nil, apierrors.NewNotFound(schema.GroupResource{}, name)
		}

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("user is disabled", func(t *testing.T) {
		oldGetUserFunc := userLister.GetFunc
		defer func() { userLister.GetFunc = oldGetUserFunc }()
		userLister.GetFunc = func(namespace, name string) (*v3.User, error) {
			return &v3.User{
				ObjectMeta: metav1.ObjectMeta{
					Name: userID,
				},
				Enabled: pointer.BoolPtr(false),
			}, nil
		}

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("error getting userattribute", func(t *testing.T) {
		oldGetUserAttributeFunc := userAttributeLister.GetFunc
		defer func() { userAttributeLister.GetFunc = oldGetUserAttributeFunc }()
		userAttributeLister.GetFunc = func(namespace, name string) (*v3.UserAttribute, error) {
			return nil, fmt.Errorf("some error")
		}

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("auth provider is disabled", func(t *testing.T) {
		oldIsDisabled := fakeProvider.disabled
		defer func() { fakeProvider.disabled = oldIsDisabled }()
		fakeProvider.disabled = true

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("auth provider doesn't exist", func(t *testing.T) {
		oldPrincipal := tokenSecret.Data[exttokenstore.FieldPrincipal]
		defer func() {
			tokenSecret.Data[exttokenstore.FieldPrincipal] = oldPrincipal
		}()
		var up v3.Principal
		json.Unmarshal(tokenSecret.Data[exttokenstore.FieldPrincipal], &up)
		up.Provider = "foo"
		tokenSecret.Data[exttokenstore.FieldPrincipal], _ = json.Marshal(up)

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("failed to verify token: token expired", func(t *testing.T) {
		oldTokenCreationTimestamp := tokenSecret.CreationTimestamp
		defer func() {
			tokenSecret.CreationTimestamp = oldTokenCreationTimestamp
		}()
		tokenSecret.CreationTimestamp = metav1.NewTime(now.
			Add(-time.Duration(token.Spec.TTL)*time.Millisecond - 1))

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("failed to verify token: mismatched 1", func(t *testing.T) {
		oldTokenHash := tokenSecret.Data[exttokenstore.FieldHash]
		defer func() {
			tokenSecret.Data[exttokenstore.FieldHash] = oldTokenHash
		}()
		misHash, _ := hashers.GetHasher().CreateHash("fkajdl;afjdlk;jaiopp;djvk")
		tokenSecret.Data[exttokenstore.FieldHash] = []byte(misHash)

		userRefresher.reset()

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})

	t.Run("failed to verify token: mismatched 2", func(t *testing.T) {
		userRefresher.reset()

		mismatchedToken := "5cncldxmczdzsqtj7kwxqldjf6dhnn5vhr42vqd6mt878wrvwnrwc8"

		req := httptest.NewRequest(http.MethodGet, "/v1/namespaces", nil)
		req.Header.Set("Authorization", "Bearer ext/"+token.Name+":"+mismatchedToken)

		resp, err := authenticator.Authenticate(req)
		require.ErrorIs(t, err, ErrMustAuthenticate)
		require.Nil(t, resp)
		assert.False(t, userRefresher.called)
	})
}
