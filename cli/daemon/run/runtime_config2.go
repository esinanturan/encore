package run

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/jackc/pgx/v5"
	"github.com/rs/xid"
	"go4.org/syncutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"encore.dev/appruntime/exported/config"
	encoreEnv "encr.dev/internal/env"
	"encr.dev/pkg/appfile"
	"encr.dev/pkg/fns"
	"encr.dev/pkg/option"
	"encr.dev/pkg/rtconfgen"
	"encr.dev/pkg/svcproxy"
	meta "encr.dev/proto/encore/parser/meta/v1"
	runtimev1 "encr.dev/proto/encore/runtime/v1"
)

const (
	runtimeCfgEnvVar    = "ENCORE_RUNTIME_CONFIG"
	appSecretsEnvVar    = "ENCORE_APP_SECRETS"
	serviceCfgEnvPrefix = "ENCORE_CFG_"
	listenEnvVar        = "ENCORE_LISTEN_ADDR"
	metaEnvVar          = "ENCORE_APP_META"
)

type RuntimeConfigGenerator struct {
	initOnce syncutil.Once
	md       *meta.Data

	// The application to generate the config for
	app interface {
		PlatformID() string
		PlatformOrLocalID() string
		GlobalCORS() (appfile.CORS, error)
		AppFile() (*appfile.File, error)
		BuildSettings() (appfile.Build, error)
	}

	// The infra manager to use
	infraManager interface {
		SQLServerConfig() (config.SQLServer, error)
		PubSubProviderConfig() (config.PubsubProvider, error)

		SQLDatabaseConfig(db *meta.SQLDatabase) (config.SQLDatabase, error)
		PubSubTopicConfig(topic *meta.PubSubTopic) (config.PubsubProvider, config.PubsubTopic, error)
		PubSubSubscriptionConfig(topic *meta.PubSubTopic, sub *meta.PubSubTopic_Subscription) (config.PubsubSubscription, error)
		RedisConfig(redis *meta.CacheCluster) (config.RedisServer, config.RedisDatabase, error)
		BucketProviderConfig() (config.BucketProvider, string, error)
	}

	AppID         option.Option[string]
	EnvID         option.Option[string]
	EnvName       option.Option[string]
	EnvType       option.Option[runtimev1.Environment_Type]
	EnvCloud      option.Option[runtimev1.Environment_Cloud]
	TraceEndpoint option.Option[string]
	DeployID      option.Option[string]
	Gateways      map[string]GatewayConfig
	AuthKey       config.EncoreAuthKey

	// Whether to include the metadata as an environment variable.
	IncludeMetaEnv bool

	// The values of defined secrets.
	DefinedSecrets map[string]string
	// The configs, per service.
	SvcConfigs map[string]string

	conf     *rtconfgen.Builder
	authKeys []*runtimev1.EncoreAuthKey
}

type GatewayConfig struct {
	BaseURL   string
	Hostnames []string
}

func (g *RuntimeConfigGenerator) initialize() error {
	return g.initOnce.Do(func() error {
		g.conf = rtconfgen.NewBuilder()

		newRid := func() string { return "res_" + xid.New().String() }

		if deployID, ok := g.DeployID.Get(); ok {
			g.conf.DeployID(deployID)
		}
		g.conf.DeployedAt(time.Now())

		g.conf.Env(&runtimev1.Environment{
			AppId:   g.AppID.GetOrElseF(g.app.PlatformOrLocalID),
			AppSlug: g.app.PlatformID(),
			EnvId:   g.EnvID.GetOrElse("local"),
			EnvName: g.EnvName.GetOrElse("local"),
			EnvType: g.EnvType.GetOrElse(runtimev1.Environment_TYPE_DEVELOPMENT),
			Cloud:   g.EnvCloud.GetOrElse(runtimev1.Environment_CLOUD_LOCAL),
		})

		toSecret := func(b []byte) *runtimev1.SecretData {
			return &runtimev1.SecretData{
				Source: &runtimev1.SecretData_Embedded{Embedded: b},
			}
		}
		ak := g.AuthKey
		g.authKeys = []*runtimev1.EncoreAuthKey{{Id: ak.KeyID, Data: toSecret(ak.Data)}}

		g.conf.EncorePlatform(&runtimev1.EncorePlatform{
			PlatformSigningKeys: g.authKeys,
			EncoreCloud:         nil,
		})

		if traceEndpoint, ok := g.TraceEndpoint.Get(); ok {
			sampleRate := 1.0
			if val, err := strconv.ParseFloat(os.Getenv("ENCORE_TRACE_SAMPLING_RATE"), 64); err == nil {
				sampleRate = min(max(val, 0), 1)
			}
			g.conf.TracingProvider(&runtimev1.TracingProvider{
				Rid: newRid(),
				Provider: &runtimev1.TracingProvider_Encore{
					Encore: &runtimev1.TracingProvider_EncoreTracingProvider{
						TraceEndpoint: traceEndpoint,
						SamplingRate:  &sampleRate,
					},
				},
			})
		}

		appFile, err := g.app.AppFile()
		if err != nil {
			return errors.Wrap(err, "failed to get app's build settings")
		}
		for _, svc := range g.md.Svcs {
			cfg := &runtimev1.HostedService{
				Name:      svc.Name,
				LogConfig: ptrOrNil(appFile.LogLevel),
			}

			if appFile.Build.WorkerPooling {
				n := int32(0)
				cfg.WorkerThreads = &n
			}
			g.conf.ServiceConfig(cfg)
		}

		g.conf.AuthMethods([]*runtimev1.ServiceAuth{
			{
				AuthMethod: &runtimev1.ServiceAuth_EncoreAuth_{
					EncoreAuth: &runtimev1.ServiceAuth_EncoreAuth{
						AuthKeys: g.authKeys,
					},
				},
			},
		})

		g.conf.DefaultGracefulShutdown(&runtimev1.GracefulShutdown{
			Total:         durationpb.New(10 * time.Second),
			ShutdownHooks: durationpb.New(4 * time.Second),
			Handlers:      durationpb.New(2 * time.Second),
		})

		for _, gw := range g.md.Gateways {
			cors, err := g.app.GlobalCORS()
			if err != nil {
				return errors.Wrap(err, "failed to generate global CORS config")
			}

			g.conf.Infra.Gateway(&runtimev1.Gateway{
				Rid:        newRid(),
				EncoreName: gw.EncoreName,
				BaseUrl:    g.Gateways[gw.EncoreName].BaseURL,
				Hostnames:  g.Gateways[gw.EncoreName].Hostnames,
				Cors: &runtimev1.Gateway_CORS{
					Debug:               cors.Debug,
					DisableCredentials:  false,
					ExtraAllowedHeaders: cors.AllowHeaders,
					ExtraExposedHeaders: cors.ExposeHeaders,

					AllowedOriginsWithCredentials: &runtimev1.Gateway_CORS_UnsafeAllowAllOriginsWithCredentials{
						UnsafeAllowAllOriginsWithCredentials: true,
					},

					AllowedOriginsWithoutCredentials: &runtimev1.Gateway_CORSAllowedOrigins{
						AllowedOrigins: []string{"*"},
					},

					AllowPrivateNetworkAccess: true,
				},
			})
		}

		if len(g.md.PubsubTopics) > 0 {
			pubsubConfig, err := g.infraManager.PubSubProviderConfig()
			if err != nil {
				return errors.Wrap(err, "failed to generate pubsub provider config")
			}

			cluster := g.conf.Infra.PubSubCluster(&runtimev1.PubSubCluster{
				Rid: newRid(),
				Provider: &runtimev1.PubSubCluster_Nsq{
					Nsq: &runtimev1.PubSubCluster_NSQ{Hosts: []string{pubsubConfig.NSQ.Host}},
				},
			})

			for _, topic := range g.md.PubsubTopics {
				topicRid := newRid()

				var deliveryGuarantee runtimev1.PubSubTopic_DeliveryGuarantee
				switch topic.DeliveryGuarantee {
				case meta.PubSubTopic_AT_LEAST_ONCE:
					deliveryGuarantee = runtimev1.PubSubTopic_DELIVERY_GUARANTEE_AT_LEAST_ONCE
				case meta.PubSubTopic_EXACTLY_ONCE:
					deliveryGuarantee = runtimev1.PubSubTopic_DELIVERY_GUARANTEE_EXACTLY_ONCE
				default:
					return errors.Newf("unknown delivery guarantee %q", topic.DeliveryGuarantee)
				}

				cluster.PubSubTopic(&runtimev1.PubSubTopic{
					Rid:               topicRid,
					EncoreName:        topic.Name,
					CloudName:         topic.Name,
					DeliveryGuarantee: deliveryGuarantee,
					OrderingAttr:      ptrOrNil(topic.OrderingKey),
					ProviderConfig:    nil,
				})

				for _, sub := range topic.Subscriptions {
					cluster.PubSubSubscription(&runtimev1.PubSubSubscription{
						Rid:                    newRid(),
						TopicEncoreName:        topic.Name,
						SubscriptionEncoreName: sub.Name,
						TopicCloudName:         topic.Name,
						SubscriptionCloudName:  sub.Name,
						PushOnly:               false,
						ProviderConfig:         nil,
					})
				}
			}
		}

		if len(g.md.SqlDatabases) > 0 {
			srvConfig, err := g.infraManager.SQLServerConfig()
			if err != nil {
				return errors.Wrap(err, "failed to generate SQL server config")
			}

			cluster := g.conf.Infra.SQLCluster(&runtimev1.SQLCluster{
				Rid: newRid(),
			})

			var tlsConfig *runtimev1.TLSConfig
			if srvConfig.ServerCACert != "" {
				tlsConfig = &runtimev1.TLSConfig{
					ServerCaCert: &srvConfig.ServerCACert,
				}
			}

			cluster.SQLServer(&runtimev1.SQLServer{
				Rid:       newRid(),
				Kind:      runtimev1.ServerKind_SERVER_KIND_PRIMARY,
				Host:      srvConfig.Host,
				TlsConfig: tlsConfig,
			})

			for _, db := range g.md.SqlDatabases {
				if externalDB, ok := g.DefinedSecrets["sqldb::"+db.Name]; ok {
					var extCfg struct {
						ConnectionString string `json:"connection_string"`
					}
					if err := json.Unmarshal([]byte(externalDB), &extCfg); err != nil {
						return errors.Wrapf(err, "failed to unmarshal external DB config for %q", db.Name)
					}
					pCfg, err := pgx.ParseConfig(extCfg.ConnectionString)
					if err != nil {
						return errors.Wrapf(err, "failed to parse external DB connection string for %q", db.Name)
					}
					cluster := g.conf.Infra.SQLCluster(&runtimev1.SQLCluster{
						Rid: newRid(),
					})
					cluster.SQLServer(&runtimev1.SQLServer{
						Rid:  newRid(),
						Kind: runtimev1.ServerKind_SERVER_KIND_PRIMARY,
						Host: pCfg.Host,
						TlsConfig: &runtimev1.TLSConfig{
							DisableCaValidation: true,
						},
					})
					// Generate a role rid based on the cluster+username combination.
					roleRid := fmt.Sprintf("role:%s:%s", cluster.Val.Rid, pCfg.User)
					g.conf.Infra.SQLRole(&runtimev1.SQLRole{
						Rid:           roleRid,
						Username:      pCfg.User,
						Password:      toSecret([]byte(pCfg.Password)),
						ClientCertRid: nil,
					})
					cluster.SQLDatabase(&runtimev1.SQLDatabase{
						Rid:        newRid(),
						EncoreName: db.Name,
						CloudName:  pCfg.Database,
						ConnPools:  nil,
					}).AddConnectionPool(&runtimev1.SQLConnectionPool{
						IsReadonly:     false,
						RoleRid:        roleRid,
						MinConnections: int32(0),
						MaxConnections: int32(0),
					})
				} else {
					dbConfig, err := g.infraManager.SQLDatabaseConfig(db)
					if err != nil {
						return errors.Wrap(err, "failed to generate SQL database config")
					}

					// Generate a role rid based on the cluster+username combination.
					roleRid := fmt.Sprintf("role:%s:%s", cluster.Val.Rid, dbConfig.User)
					g.conf.Infra.SQLRole(&runtimev1.SQLRole{
						Rid:           roleRid,
						Username:      dbConfig.User,
						Password:      toSecret([]byte(dbConfig.Password)),
						ClientCertRid: nil,
					})
					cluster.SQLDatabase(&runtimev1.SQLDatabase{
						Rid:        newRid(),
						EncoreName: dbConfig.EncoreName,
						CloudName:  dbConfig.DatabaseName,
						ConnPools:  nil,
					}).AddConnectionPool(&runtimev1.SQLConnectionPool{
						IsReadonly:     false,
						RoleRid:        roleRid,
						MinConnections: int32(dbConfig.MinConnections),
						MaxConnections: int32(dbConfig.MaxConnections),
					})

				}

			}
		}

		if len(g.md.CacheClusters) > 0 {
			for _, cl := range g.md.CacheClusters {
				srvConfig, dbConfig, err := g.infraManager.RedisConfig(cl)
				if err != nil {
					return errors.Wrap(err, "failed to generate Redis cluster config")
				}

				cluster := g.conf.Infra.RedisCluster(&runtimev1.RedisCluster{
					Rid:     newRid(),
					Servers: nil,
				})

				// Generate a role rid based on the cluster+username combination.
				roleRid := fmt.Sprintf("role:%s:%s", cluster.Val.Rid, srvConfig.User)
				g.conf.Infra.RedisRoleFn(roleRid, func() *runtimev1.RedisRole {
					r := &runtimev1.RedisRole{
						Rid:           roleRid,
						ClientCertRid: nil,
					}
					switch {
					case srvConfig.User != "" && srvConfig.Password != "":
						r.Auth = &runtimev1.RedisRole_Acl{Acl: &runtimev1.RedisRole_AuthACL{
							Username: srvConfig.User,
							Password: toSecret([]byte(srvConfig.Password)),
						}}
					case srvConfig.Password != "":
						r.Auth = &runtimev1.RedisRole_AuthString{AuthString: toSecret([]byte(srvConfig.Password))}
					default:
						r.Auth = nil
					}
					return r
				})

				var tlsConfig *runtimev1.TLSConfig
				if srvConfig.EnableTLS || srvConfig.ServerCACert != "" {
					tlsConfig = &runtimev1.TLSConfig{
						ServerCaCert: ptrOrNil(srvConfig.ServerCACert),
					}
				}

				cluster.RedisServer(&runtimev1.RedisServer{
					Rid:       newRid(),
					Host:      srvConfig.Host,
					Kind:      runtimev1.ServerKind_SERVER_KIND_PRIMARY,
					TlsConfig: tlsConfig,
				})
				cluster.RedisDatabase(&runtimev1.RedisDatabase{
					Rid:         newRid(),
					EncoreName:  dbConfig.EncoreName,
					DatabaseIdx: int32(dbConfig.Database),
					KeyPrefix:   ptrOrNil(dbConfig.KeyPrefix),
					ConnPools:   nil,
				}).AddConnectionPool(&runtimev1.RedisConnectionPool{
					IsReadonly:     false,
					RoleRid:        roleRid,
					MinConnections: int32(dbConfig.MinConnections),
					MaxConnections: int32(dbConfig.MaxConnections),
				})
			}
		}

		if len(g.md.Buckets) > 0 {
			bktProviderConfig, publicBaseURL, err := g.infraManager.BucketProviderConfig()
			if err != nil {
				return errors.Wrap(err, "failed to generate bucket provider config")
			}

			cluster := g.conf.Infra.BucketCluster(&runtimev1.BucketCluster{
				Rid: newRid(),
				Provider: &runtimev1.BucketCluster_Gcs{
					Gcs: &runtimev1.BucketCluster_GCS{
						Endpoint:  &bktProviderConfig.GCS.Endpoint,
						Anonymous: true,
						LocalSign: &runtimev1.BucketCluster_GCS_LocalSignOptions{
							BaseUrl:    publicBaseURL,
							AccessId:   "dummy-sa@encore.local",
							PrivateKey: reverseString(dummyPrivateKeyReversed),
						},
					},
				},
			})

			for _, bkt := range g.md.Buckets {
				bktRid := newRid()

				var publicURL *string
				if bkt.Public {
					u := publicBaseURL + "/" + bkt.Name
					publicURL = &u
				}
				cluster.Bucket(&runtimev1.Bucket{
					Rid:           bktRid,
					EncoreName:    bkt.Name,
					CloudName:     bkt.Name,
					PublicBaseUrl: publicURL,
				})
			}
		}

		for secretName, secretVal := range g.DefinedSecrets {
			g.conf.Infra.AppSecret(&runtimev1.AppSecret{
				Rid:        newRid(),
				EncoreName: secretName,
				Data:       toSecret([]byte(secretVal)),
			})
		}

		return nil
	})
}

type ProcConfig struct {
	// The runtime config to add to the process, if any.
	Runtime option.Option[*runtimev1.RuntimeConfig]

	ListenAddr netip.AddrPort
	ExtraEnv   []string
}

func (g *RuntimeConfigGenerator) ProcPerService(proxy *svcproxy.SvcProxy) (services, gateways map[string]*ProcConfig, err error) {
	if err := g.initialize(); err != nil {
		return nil, nil, err
	}

	services = make(map[string]*ProcConfig)
	gateways = make(map[string]*ProcConfig)

	newRid := func() string { return "res_" + xid.New().String() }

	sd := &runtimev1.ServiceDiscovery{Services: make(map[string]*runtimev1.ServiceDiscovery_Location)}

	svcListenAddr := make(map[string]netip.AddrPort)
	for _, svc := range g.md.Svcs {
		listenAddr, err := freeLocalhostAddress()
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed to find free localhost address")
		}
		svcListenAddr[svc.Name] = listenAddr
		sd.Services[svc.Name] = &runtimev1.ServiceDiscovery_Location{
			BaseUrl: proxy.RegisterService(svc.Name, listenAddr),
			AuthMethods: []*runtimev1.ServiceAuth{
				{
					AuthMethod: &runtimev1.ServiceAuth_EncoreAuth_{
						EncoreAuth: &runtimev1.ServiceAuth_EncoreAuth{
							AuthKeys: g.authKeys,
						},
					},
				},
			},
		}
	}

	// Set up the service processes.
	for _, svc := range g.md.Svcs {
		conf, err := g.conf.Deployment(newRid()).
			ServiceDiscovery(sd).
			HostsServices(svc.Name).
			ReduceWithMeta(g.md).
			BuildRuntimeConfig()
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed to generate runtime config")
		}

		usedSecrets := secretsUsedByServices(g.md, svc.Name)
		listenAddr := svcListenAddr[svc.Name]
		configEnvs := g.encodeConfigs(svc.Name)

		services[svc.Name] = &ProcConfig{
			Runtime:    option.Some(conf),
			ListenAddr: listenAddr,
			ExtraEnv: append([]string{
				fmt.Sprintf("%s=%s", appSecretsEnvVar, g.encodeSecrets(usedSecrets)),
			}, configEnvs...),
		}
	}

	// Set up the gateways.
	for _, gw := range g.md.Gateways {
		conf, err := g.conf.Deployment(newRid()).ServiceDiscovery(sd).HostsGateways(gw.EncoreName).ReduceWithMeta(g.md).BuildRuntimeConfig()
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed to generate runtime config")
		}
		listenAddr, err := freeLocalhostAddress()
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed to find free localhost address")
		}
		gateways[gw.EncoreName] = &ProcConfig{
			Runtime:    option.Some(conf),
			ListenAddr: listenAddr,
			ExtraEnv:   []string{},
		}
	}

	return
}

func (g *RuntimeConfigGenerator) AllInOneProc() (*ProcConfig, error) {
	if err := g.initialize(); err != nil {
		return nil, err
	}

	newRid := func() string { return "res_" + xid.New().String() }

	sd := &runtimev1.ServiceDiscovery{Services: make(map[string]*runtimev1.ServiceDiscovery_Location)}

	d := g.conf.Deployment(newRid()).ServiceDiscovery(sd)
	for _, gw := range g.md.Gateways {
		d.HostsGateways(gw.EncoreName)
	}
	for _, svc := range g.md.Svcs {
		d.HostsServices(svc.Name)
	}

	conf, err := d.ReduceWithMeta(g.md).BuildRuntimeConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate runtime config")
	}

	listenAddr, err := freeLocalhostAddress()
	if err != nil {
		return nil, errors.Wrap(err, "failed to find free localhost address")
	}

	configEnvs := g.encodeConfigs(fns.Map(g.md.Svcs, func(svc *meta.Service) string { return svc.Name })...)

	return &ProcConfig{
		Runtime:    option.Some(conf),
		ListenAddr: listenAddr,
		ExtraEnv: append([]string{
			fmt.Sprintf("%s=%s", appSecretsEnvVar, encodeSecretsEnv(g.DefinedSecrets)),
		}, configEnvs...),
	}, nil
}

func (g *RuntimeConfigGenerator) ProcPerServiceWithNewRuntimeConfig(proxy *svcproxy.SvcProxy) (conf *runtimev1.RuntimeConfig, services, gateways map[string]*ProcConfig, err error) {
	if err := g.initialize(); err != nil {
		return nil, nil, nil, err
	}

	if len(g.SvcConfigs) > 0 {
		return nil, nil, nil, errors.New("service configs not yet supported")
	}

	services = make(map[string]*ProcConfig)
	gateways = make(map[string]*ProcConfig)

	newRid := func() string { return "res_" + xid.New().String() }

	sd := &runtimev1.ServiceDiscovery{Services: make(map[string]*runtimev1.ServiceDiscovery_Location)}

	svcListenAddr := make(map[string]netip.AddrPort)
	var svcNames []string
	for _, svc := range g.md.Svcs {
		svcNames = append(svcNames, svc.Name)
		listenAddr, err := freeLocalhostAddress()
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "failed to find free localhost address")
		}
		svcListenAddr[svc.Name] = listenAddr
		sd.Services[svc.Name] = &runtimev1.ServiceDiscovery_Location{
			BaseUrl: proxy.RegisterService(svc.Name, listenAddr),
			AuthMethods: []*runtimev1.ServiceAuth{
				{
					AuthMethod: &runtimev1.ServiceAuth_EncoreAuth_{
						EncoreAuth: &runtimev1.ServiceAuth_EncoreAuth{
							AuthKeys: g.authKeys,
						},
					},
				},
			},
		}
	}

	for _, svc := range g.md.Svcs {
		conf, err = g.conf.Deployment(newRid()).
			ServiceDiscovery(sd).
			HostsServices(svc.Name).
			ReduceWithMeta(g.md).
			BuildRuntimeConfig()
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "failed to generate runtime config")
		}

		listenAddr := svcListenAddr[svc.Name]
		services[svc.Name] = &ProcConfig{
			Runtime:    option.Some(conf),
			ListenAddr: listenAddr,
		}
	}

	// Set up the gateways.
	for _, gw := range g.md.Gateways {
		listenAddr, err := freeLocalhostAddress()
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "failed to find free localhost address")
		}

		conf, err = g.conf.Deployment(newRid()).
			ServiceDiscovery(sd).
			HostsGateways(gw.EncoreName).
			//ReduceWithMeta(g.md).
			BuildRuntimeConfig()
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "failed to generate runtime config")
		}
		gateways[gw.EncoreName] = &ProcConfig{
			Runtime:    option.Some(conf),
			ListenAddr: listenAddr,
		}
	}

	return
}

func (g *RuntimeConfigGenerator) ForTests(newRuntimeConf bool) (envs []string, err error) {
	if err := g.initialize(); err != nil {
		return nil, err
	}

	newRid := func() string { return "res_" + xid.New().String() }

	sd := &runtimev1.ServiceDiscovery{Services: make(map[string]*runtimev1.ServiceDiscovery_Location)}

	d := g.conf.Deployment(newRid()).ServiceDiscovery(sd)
	for _, gw := range g.md.Gateways {
		d.HostsGateways(gw.EncoreName)
	}
	for _, svc := range g.md.Svcs {
		d.HostsServices(svc.Name)
	}

	conf, err := d.ReduceWithMeta(g.md).BuildRuntimeConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate runtime config")
	}

	var runtimeCfgStr string
	if newRuntimeConf {
		runtimeCfgBytes, err := proto.Marshal(conf)
		if err != nil {
			return nil, errors.Wrap(err, "failed to marshal runtime config")
		}
		gzipped := gzipBytes(runtimeCfgBytes)
		runtimeCfgStr = "gzip:" + base64.StdEncoding.EncodeToString(gzipped)
	} else {
		// We don't use secretEnvs because for local development we use
		// plaintext secrets across the board.
		var secretEnvs map[string][]byte = nil

		runtimeCfg, err := rtconfgen.ToLegacy(conf, secretEnvs)
		if err != nil {
			return nil, errors.Wrap(err, "failed to generate runtime config")
		}
		runtimeCfgBytes, err := json.Marshal(runtimeCfg)
		if err != nil {
			return nil, errors.Wrap(err, "failed to marshal runtime config")
		}
		runtimeCfgStr = base64.RawURLEncoding.EncodeToString(runtimeCfgBytes)
	}

	envs = append(envs,
		fmt.Sprintf("%s=%s", appSecretsEnvVar, encodeSecretsEnv(g.DefinedSecrets)),
		fmt.Sprintf("%s=%s", runtimeCfgEnvVar, runtimeCfgStr),
	)

	svcNames := fns.Map(g.md.Svcs, func(svc *meta.Service) string { return svc.Name })
	envs = append(envs, g.encodeConfigs(svcNames...)...)

	if g.IncludeMetaEnv {
		metaBytes, err := proto.Marshal(g.md)
		if err != nil {
			return nil, errors.Wrap(err, "failed to marshal metadata")
		}
		gzipped := gzipBytes(metaBytes)
		metaEnvStr := "gzip:" + base64.StdEncoding.EncodeToString(gzipped)
		envs = append(envs, fmt.Sprintf("%s=%s", metaEnvVar, metaEnvStr))
	}

	if runtimeLibPath := encoreEnv.EncoreRuntimeLib(); runtimeLibPath != "" {
		envs = append(envs, "ENCORE_RUNTIME_LIB="+runtimeLibPath)
	}

	return envs, nil
}

func ptrOrNil[T comparable](val T) *T {
	var zero T
	if val == zero {
		return nil
	}
	return &val
}

func (g *RuntimeConfigGenerator) ProcEnvs(proc *ProcConfig, useRuntimeConfigV2 bool) ([]string, error) {
	env := append([]string{
		fmt.Sprintf("%s=%s", listenEnvVar, proc.ListenAddr.String()),
	}, proc.ExtraEnv...)

	if rt, ok := proc.Runtime.Get(); ok {
		var runtimeCfgStr string

		if useRuntimeConfigV2 {
			runtimeCfgBytes, err := proto.Marshal(rt)
			if err != nil {
				return nil, errors.Wrap(err, "failed to marshal runtime config")
			}
			gzipped := gzipBytes(runtimeCfgBytes)
			runtimeCfgStr = "gzip:" + base64.StdEncoding.EncodeToString(gzipped)
		} else {
			// We don't use secretEnvs because for local development we use
			// plaintext secrets across the board.
			var secretEnvs map[string][]byte = nil

			runtimeCfg, err := rtconfgen.ToLegacy(rt, secretEnvs)
			if err != nil {
				return nil, errors.Wrap(err, "failed to generate runtime config")
			}

			runtimeCfgBytes, err := json.Marshal(runtimeCfg)
			if err != nil {
				return nil, errors.Wrap(err, "failed to marshal runtime config")
			}
			runtimeCfgStr = base64.RawURLEncoding.EncodeToString(runtimeCfgBytes)
		}

		env = append(env, fmt.Sprintf("%s=%s", runtimeCfgEnvVar, runtimeCfgStr))
	}

	if g.IncludeMetaEnv {
		metaBytes, err := proto.Marshal(g.md)
		if err != nil {
			return nil, errors.Wrap(err, "failed to marshal metadata")
		}
		gzipped := gzipBytes(metaBytes)
		metaEnvStr := "gzip:" + base64.StdEncoding.EncodeToString(gzipped)
		env = append(env, fmt.Sprintf("%s=%s", metaEnvVar, metaEnvStr))
	}

	if runtimeLibPath := encoreEnv.EncoreRuntimeLib(); runtimeLibPath != "" {
		env = append(env, "ENCORE_RUNTIME_LIB="+runtimeLibPath)
	}

	return env, nil
}

func (g *RuntimeConfigGenerator) MissingSecrets() []string {
	var missing []string
	for _, pkg := range g.md.Pkgs {
		for _, name := range pkg.Secrets {
			if _, ok := g.DefinedSecrets[name]; !ok {
				missing = append(missing, name)
			}
		}
	}

	sort.Strings(missing)
	missing = slices.Compact(missing)
	return missing
}

func (g *RuntimeConfigGenerator) encodeSecrets(secretNames map[string]bool) string {
	vals := make(map[string]string)
	for name := range secretNames {
		vals[name] = g.DefinedSecrets[name]
	}
	return encodeSecretsEnv(vals)
}

func (g *RuntimeConfigGenerator) encodeConfigs(svcNames ...string) []string {
	envs := make([]string, 0, len(svcNames))
	for _, svcName := range svcNames {
		cfgStr, ok := g.SvcConfigs[svcName]
		if !ok {
			continue
		}
		envs = append(envs,
			fmt.Sprintf(
				"%s%s=%s",
				serviceCfgEnvPrefix,
				strings.ToUpper(svcName),
				base64.RawURLEncoding.EncodeToString([]byte(cfgStr)),
			),
		)
	}

	return envs
}

// secretsUsedByServices returns the set of secrets that are accessible by the given services, using the metadata for access control.
func secretsUsedByServices(md *meta.Data, svcNames ...string) (secretNames map[string]bool) {
	svcNameSet := make(map[string]bool)
	for _, name := range svcNames {
		svcNameSet[name] = true
	}

	secretNames = make(map[string]bool)
	for _, pkg := range md.Pkgs {
		if len(pkg.Secrets) > 0 && (pkg.ServiceName == "" || svcNameSet[pkg.ServiceName]) {
			for _, secret := range pkg.Secrets {
				secretNames[secret] = true
			}
		}
	}
	return secretNames
}

// freeLocalhostAddress returns the first free port number on the system.
func freeLocalhostAddress() (netip.AddrPort, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return netip.AddrPort{}, err
	}
	defer func() { _ = l.Close() }()

	return l.Addr().(*net.TCPAddr).AddrPort(), nil
}

func encodeServiceConfigs(svcCfgs map[string]string) []string {
	envs := make([]string, 0, len(svcCfgs))
	for serviceName, cfgString := range svcCfgs {
		envs = append(envs, "ENCORE_CFG_"+strings.ToUpper(serviceName)+"="+base64.RawURLEncoding.EncodeToString([]byte(cfgString)))
	}
	slices.Sort(envs)
	return envs
}

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(data)
	_ = w.Close()
	return buf.Bytes()
}

func reverseString(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

// We lightly obfuscate the PK to trigger fewer of the tools that warn about
// keys in source code.
//
// $ tail -r pk.pem | rev
const dummyPrivateKeyReversed = `-----YEK ETAVIRP DNE-----
=AOz3eEM5xAe/71Tfx3sQNkW
4FXBCChkppSrCoQnR6pBeP31wu0S0UTTNDhNmSYcerdSFbRhyZOzNRnhF9o1h5D5
+gKkhRZkC33z5+0p8aWwOVWJY8MDycHwvEYvtwcXLNZBHI8L8++mhp0uFz5c5sNM
pPRyurcUY36iDzx7hAJcAGoAvXJwVzTmzXBZtvFPs6Alc5gHti2W1l2bz2mwOV77
BA9xAW4R6EHVTnqaoXvxvocW5Z9I0ecJzx0NPfkXBriW1lNclAnkoRAYqziasa6C
WIxePQ2VRFbnLu7XR1M/xqg00GHFV0fTlNPo95lC6tl0PAdoupOX1lwjH3rQnTkB
Y4BgBKQQJ8F0PPTSMAvyK1bcHP2Iob8UFxyHuPOm11aHYwM4VZvmHm8jX/8vz4eb
6kbNbEkWzfJbbEen/EJLR1XtzvTdjs9bQnJvhQMZmPGzQalqHcVuilQX+PFV4ezM
A23w1HCIq6vZqXLO8rXhe8S5hImwVSAKq6TK5dlYPOTIBp66lCQgBKwjkcQcX7tq
mr44FuVB7hqBMfnCB0kKcs1SuYgmfUQE41JGInsqjdpaFOwzQi4Jcx7TK44p9vn2
ik6i/hN7JSVA8kMImWIxtL18uVC/Rg0RpM2vcjd+pfgUDifZ1FVYCiL3WlEzDBlZ
bSmYdd57T70mEEiuV8QmGiIRrk6kZAMP4CQgBKQ4mIYJX2RJQ1j0V+iXwY/bg+N5
DPEWLB0w6ReZapNy4DSEMD1zm6IWUuo3rGfCsSKUD0xFR/YkauO5Q+GI2gKvmj5V
MRiysBL/8PCBwKiFKo1MFjCUfbV/ks49/OJYSOi9WIJiXEg5Tm56BDTH6I8rNdU1
lGIimbKIuzEBWUHsyDQgBKQQ8O/PDCI/SJSPYjkxw1fpX022hUvVW9pvtmd6v0vX
M5kMBkT60IwTWhF0DoAx4Uyn4rlPiJy5TUwjC0po/aCRV+ug5C+wIRTCtVCpqRyz
GeB4U/3WXHmSulzK5Dw4ADfbWSP0dAbNNOaFI4y6u+acEl5MFt3GN/jieITLsZNK
X18B7zHj7LR2f5k3xiJJ/7uNFl8SCcnVquvEI1qslUSTLEPCNoiy5iX/VVTmVNwv
dUi92s5oFMyJOFW5joggeeQ55BN6EsjQTnj/XetnpPe5wf5vvptHg5HOcUjJPmIJ
vsGpMXoyCh3mzdQPMUJM9Ha8DKlACadqTjdid9ZsAAYLAEggCEAABMgAvulUiO2B
FkdtezbN/f5vpPbr4knO22xylfkUp5Uw0W/HxtntXXobF42guEEiie49zki5fPHK
vAMC7bOERRLV4v35Dd9QV/KFe0FxqEfm8bFDM6FoA4c0qnkDaKbMhdvxxs0wVFRm
BukfBCLOt+W/XyFhZvUKkxgbcOjXV7HRFQGI+GZnrf00qbCRNOCdlYLoYX1kf3pQ
eNY6o9ZCJxIDO+dUATCoP3tmP4hvonrjGfpek99D4Ye3+iDwg0AxDW+bt9qoRFew
VdOuGmooPaDDxn95q5IghRhrvrEaHpkN/EZiNEAJWQkZa9wkxGye5T9hMZRBjUkt
wGPTyf02fuGquCQABIoAAEgAjSggwcKBCSAAFEQAB0w9GikhqkgBNADABIQvEIIM
-----YEK ETAVIRP NIGEB-----`
