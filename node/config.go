package node

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Layr-Labs/eigenda/common"
	"github.com/Layr-Labs/eigenda/common/geth"
	"github.com/Layr-Labs/eigenda/core"
	"github.com/Layr-Labs/eigenda/encoding/kzg"
	"github.com/Layr-Labs/eigenda/node/flags"

	blssignerTypes "github.com/Layr-Labs/eigensdk-go/signer/bls/types"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfave/cli"
)

const (
	// Min number of seconds for the ExpirationPollIntervalSecFlag.
	minExpirationPollIntervalSec   = 3
	minReachabilityPollIntervalSec = 10
	AppName                        = "da-node"
)

var (
	// QuorumNames maps quorum IDs to their names.
	// this is used for eigen metrics
	QuorumNames = map[core.QuorumID]string{
		0: "eth_quorum",
		1: "eignen_quorum",
	}
	SemVer    = "0.0.0"
	GitCommit = ""
	GitDate   = ""
)

// Config contains all of the configuration information for a DA node.
type Config struct {
	Hostname                       string
	RetrievalPort                  string
	DispersalPort                  string
	InternalRetrievalPort          string
	InternalDispersalPort          string
	V2DispersalPort                string
	EnableNodeApi                  bool
	NodeApiPort                    string
	EnableMetrics                  bool
	MetricsPort                    int
	OnchainMetricsInterval         int64
	Timeout                        time.Duration
	RegisterNodeAtStart            bool
	ExpirationPollIntervalSec      uint64
	EnableTestMode                 bool
	OverrideBlockStaleMeasure      int64
	OverrideStoreDurationBlocks    int64
	QuorumIDList                   []core.QuorumID
	DbPath                         string
	LogPath                        string
	ID                             core.OperatorID
	BLSOperatorStateRetrieverAddr  string
	EigenDAServiceManagerAddr      string
	PubIPProviders                 []string
	PubIPCheckInterval             time.Duration
	ChurnerUrl                     string
	DataApiUrl                     string
	NumBatchValidators             int
	NumBatchDeserializationWorkers int
	EnableGnarkBundleEncoding      bool
	ClientIPHeader                 string
	UseSecureGrpc                  bool
	ReachabilityPollIntervalSec    uint64
	DisableNodeInfoResources       bool

	BlsSignerConfig blssignerTypes.SignerConfig

	EthClientConfig geth.EthClientConfig
	LoggerConfig    common.LoggerConfig
	EncoderConfig   kzg.KzgConfig

	EnableV2                    bool
	OnchainStateRefreshInterval time.Duration
	ChunkDownloadTimeout        time.Duration
	GRPCMsgSizeLimitV2          int

	PprofHttpPort string
	EnablePprof   bool

	// if true then the node will not authenticate StoreChunks requests from dispersers (v2 only)
	DisableDispersalAuthentication bool
	// the size of the cache for storing public keys of dispersers
	DispersalAuthenticationKeyCacheSize int
	// the timeout for disperser keys (after which the disperser key is reloaded from the chain)
	DisperserKeyTimeout time.Duration
	// the timeout for disperser authentication (set to 0 to disable), if enabled then a successful authentication
	// of a StoreChunks request causes the node to skip validation for requests coming from the same IP address
	// for this duration. Adds risk of disruptive behavior if an attacker is able to send requests from the same IP
	// address as a legitimate disperser, but reduces performance overhead of StoreChunks validation.
	DispersalAuthenticationTimeout time.Duration
}

// NewConfig parses the Config from the provided flags or environment variables and
// returns a Config.
func NewConfig(ctx *cli.Context) (*Config, error) {
	timeout, err := time.ParseDuration(ctx.GlobalString(flags.TimeoutFlag.Name))
	if err != nil {
		return &Config{}, err
	}

	idsStr := strings.Split(ctx.GlobalString(flags.QuorumIDListFlag.Name), ",")
	ids := make([]core.QuorumID, 0)
	for _, id := range idsStr {
		val, err := strconv.Atoi(id)
		if err != nil {
			return nil, err
		}
		ids = append(ids, core.QuorumID(val))
	}
	if len(ids) == 0 {
		return nil, errors.New("no quorum ids provided")
	}

	expirationPollIntervalSec := ctx.GlobalUint64(flags.ExpirationPollIntervalSecFlag.Name)
	if expirationPollIntervalSec < minExpirationPollIntervalSec {
		return nil, fmt.Errorf("the expiration-poll-interval flag must be >= %d seconds", minExpirationPollIntervalSec)
	}

	reachabilityPollIntervalSec := ctx.GlobalUint64(flags.ReachabilityPollIntervalSecFlag.Name)
	if reachabilityPollIntervalSec != 0 && reachabilityPollIntervalSec < minReachabilityPollIntervalSec {
		return nil, fmt.Errorf("the reachability-poll-interval flag must be >= %d seconds or 0 to disable", minReachabilityPollIntervalSec)
	}

	testMode := ctx.GlobalBool(flags.EnableTestModeFlag.Name)

	// Configuration options that require the Node Operator ECDSA key at runtime
	registerNodeAtStart := ctx.GlobalBool(flags.RegisterAtNodeStartFlag.Name)
	pubIPCheckInterval := ctx.GlobalDuration(flags.PubIPCheckIntervalFlag.Name)
	needECDSAKey := registerNodeAtStart || pubIPCheckInterval > 0
	if registerNodeAtStart && (ctx.GlobalString(flags.EcdsaKeyFileFlag.Name) == "" || ctx.GlobalString(flags.EcdsaKeyPasswordFlag.Name) == "") {
		return nil, fmt.Errorf("%s and %s are required if %s is enabled", flags.EcdsaKeyFileFlag.Name, flags.EcdsaKeyPasswordFlag.Name, flags.RegisterAtNodeStartFlag.Name)
	}
	if pubIPCheckInterval > 0 && (ctx.GlobalString(flags.EcdsaKeyFileFlag.Name) == "" || ctx.GlobalString(flags.EcdsaKeyPasswordFlag.Name) == "") {
		return nil, fmt.Errorf("%s and %s are required if %s is > 0", flags.EcdsaKeyFileFlag.Name, flags.EcdsaKeyPasswordFlag.Name, flags.PubIPCheckIntervalFlag.Name)
	}

	var ethClientConfig geth.EthClientConfig
	if !testMode {
		ethClientConfig = geth.ReadEthClientConfigRPCOnly(ctx)
		if needECDSAKey {
			// Decrypt ECDSA key
			keyContents, err := os.ReadFile(ctx.GlobalString(flags.EcdsaKeyFileFlag.Name))
			if err != nil {
				return nil, fmt.Errorf("could not read ECDSA key file: %v", err)
			}
			sk, err := keystore.DecryptKey(keyContents, ctx.GlobalString(flags.EcdsaKeyPasswordFlag.Name))
			if err != nil {
				return nil, fmt.Errorf("could not decrypt the ECDSA file: %s", ctx.GlobalString(flags.EcdsaKeyFileFlag.Name))
			}
			ethClientConfig.PrivateKeyString = fmt.Sprintf("%x", crypto.FromECDSA(sk.PrivateKey))
		}
	} else {
		ethClientConfig = geth.ReadEthClientConfig(ctx)
	}

	var blsSignerConfig blssignerTypes.SignerConfig
	if !testMode {
		blsSignerCertFilePath := ctx.GlobalString(flags.BLSSignerCertFileFlag.Name)
		enableTLS := len(blsSignerCertFilePath) > 0
		signerType := blssignerTypes.Local

		// check if BLS remote signer configuration is provided
		blsRemoteSignerEnabled := ctx.GlobalBool(flags.BLSRemoteSignerEnabledFlag.Name)
		blsRemoteSignerUrl := ctx.GlobalString(flags.BLSRemoteSignerUrlFlag.Name)
		blsPublicKeyHex := ctx.GlobalString(flags.BLSPublicKeyHexFlag.Name)
		blsKeyFilePath := ctx.GlobalString(flags.BlsKeyFileFlag.Name)
		blsKeyPassword := ctx.GlobalString(flags.BlsKeyPasswordFlag.Name)
		blsSignerAPIKey := ctx.GlobalString(flags.BLSSignerAPIKeyFlag.Name)

		if blsRemoteSignerEnabled && (blsRemoteSignerUrl == "" || blsPublicKeyHex == "") {
			return nil, fmt.Errorf("BLS remote signer URL and Public Key Hex is required if BLS remote signer is enabled")
		}
		if !blsRemoteSignerEnabled && (blsKeyFilePath == "" || blsKeyPassword == "") {
			return nil, fmt.Errorf("BLS key file and password is required if BLS remote signer is disabled")
		}

		if blsRemoteSignerEnabled && blsSignerAPIKey == "" {
			return nil, fmt.Errorf("BLS signer API key is required if BLS remote signer is enabled")
		}

		if blsRemoteSignerEnabled {
			signerType = blssignerTypes.Cerberus
		}

		blsSignerConfig = blssignerTypes.SignerConfig{
			SignerType:       signerType,
			Path:             blsKeyFilePath,
			Password:         blsKeyPassword,
			CerberusUrl:      blsRemoteSignerUrl,
			PublicKeyHex:     blsPublicKeyHex,
			CerberusPassword: blsKeyPassword,
			EnableTLS:        enableTLS,
			TLSCertFilePath:  ctx.GlobalString(flags.BLSSignerCertFileFlag.Name),
			CerberusAPIKey:   blsSignerAPIKey,
		}
	} else {
		privateBls := ctx.GlobalString(flags.TestPrivateBlsFlag.Name)
		blsSignerConfig = blssignerTypes.SignerConfig{
			SignerType: blssignerTypes.PrivateKey,
			PrivateKey: privateBls,
		}
	}

	internalDispersalFlag := ctx.GlobalString(flags.InternalDispersalPortFlag.Name)
	internalRetrievalFlag := ctx.GlobalString(flags.InternalRetrievalPortFlag.Name)
	if internalDispersalFlag == "" {
		internalDispersalFlag = ctx.GlobalString(flags.DispersalPortFlag.Name)
	}
	if internalRetrievalFlag == "" {
		internalRetrievalFlag = ctx.GlobalString(flags.RetrievalPortFlag.Name)
	}

	loggerConfig, err := common.ReadLoggerCLIConfig(ctx, flags.FlagPrefix)
	if err != nil {
		return nil, err
	}

	// check if the ports are valid integers
	dispersalPort := ctx.GlobalString(flags.DispersalPortFlag.Name)
	_, err = strconv.Atoi(dispersalPort)
	if err != nil {
		return nil, fmt.Errorf("invalid dispersal port: %s", dispersalPort)
	}

	retrievalPort := ctx.GlobalString(flags.RetrievalPortFlag.Name)
	_, err = strconv.Atoi(retrievalPort)
	if err != nil {
		return nil, fmt.Errorf("invalid retrieval port: %s", retrievalPort)
	}

	v2Enabled := ctx.GlobalBool(flags.EnableV2Flag.Name)

	v2DispersalPort := ctx.GlobalString(flags.V2DispersalPortFlag.Name)
	if v2DispersalPort == "" {
		if v2Enabled {
			return nil, fmt.Errorf("v2 dispersal port (NODE_V2_DISPERSAL_PORT) must be specified if v2 is enabled")
		}
	} else {
		_, err = strconv.Atoi(v2DispersalPort)
		if err != nil {
			return nil, fmt.Errorf("invalid v2 dispersal port: %s", v2DispersalPort)
		}
	}

	return &Config{
		Hostname:                            ctx.GlobalString(flags.HostnameFlag.Name),
		DispersalPort:                       dispersalPort,
		RetrievalPort:                       retrievalPort,
		InternalDispersalPort:               internalDispersalFlag,
		InternalRetrievalPort:               internalRetrievalFlag,
		V2DispersalPort:                     v2DispersalPort,
		EnableNodeApi:                       ctx.GlobalBool(flags.EnableNodeApiFlag.Name),
		NodeApiPort:                         ctx.GlobalString(flags.NodeApiPortFlag.Name),
		EnableMetrics:                       ctx.GlobalBool(flags.EnableMetricsFlag.Name),
		MetricsPort:                         ctx.GlobalInt(flags.MetricsPortFlag.Name),
		OnchainMetricsInterval:              ctx.GlobalInt64(flags.OnchainMetricsIntervalFlag.Name),
		Timeout:                             timeout,
		RegisterNodeAtStart:                 registerNodeAtStart,
		ExpirationPollIntervalSec:           expirationPollIntervalSec,
		ReachabilityPollIntervalSec:         reachabilityPollIntervalSec,
		EnableTestMode:                      testMode,
		OverrideBlockStaleMeasure:           ctx.GlobalInt64(flags.OverrideBlockStaleMeasureFlag.Name),
		OverrideStoreDurationBlocks:         ctx.GlobalInt64(flags.OverrideStoreDurationBlocksFlag.Name),
		QuorumIDList:                        ids,
		DbPath:                              ctx.GlobalString(flags.DbPathFlag.Name),
		EthClientConfig:                     ethClientConfig,
		EncoderConfig:                       kzg.ReadCLIConfig(ctx),
		LoggerConfig:                        *loggerConfig,
		BLSOperatorStateRetrieverAddr:       ctx.GlobalString(flags.BlsOperatorStateRetrieverFlag.Name),
		EigenDAServiceManagerAddr:           ctx.GlobalString(flags.EigenDAServiceManagerFlag.Name),
		PubIPProviders:                      ctx.GlobalStringSlice(flags.PubIPProviderFlag.Name),
		PubIPCheckInterval:                  pubIPCheckInterval,
		ChurnerUrl:                          ctx.GlobalString(flags.ChurnerUrlFlag.Name),
		DataApiUrl:                          ctx.GlobalString(flags.DataApiUrlFlag.Name),
		NumBatchValidators:                  ctx.GlobalInt(flags.NumBatchValidatorsFlag.Name),
		NumBatchDeserializationWorkers:      ctx.GlobalInt(flags.NumBatchDeserializationWorkersFlag.Name),
		EnableGnarkBundleEncoding:           ctx.Bool(flags.EnableGnarkBundleEncodingFlag.Name),
		ClientIPHeader:                      ctx.GlobalString(flags.ClientIPHeaderFlag.Name),
		UseSecureGrpc:                       ctx.GlobalBoolT(flags.ChurnerUseSecureGRPC.Name),
		DisableNodeInfoResources:            ctx.GlobalBool(flags.DisableNodeInfoResourcesFlag.Name),
		BlsSignerConfig:                     blsSignerConfig,
		EnableV2:                            v2Enabled,
		OnchainStateRefreshInterval:         ctx.GlobalDuration(flags.OnchainStateRefreshIntervalFlag.Name),
		ChunkDownloadTimeout:                ctx.GlobalDuration(flags.ChunkDownloadTimeoutFlag.Name),
		GRPCMsgSizeLimitV2:                  ctx.GlobalInt(flags.GRPCMsgSizeLimitV2Flag.Name),
		PprofHttpPort:                       ctx.GlobalString(flags.PprofHttpPort.Name),
		EnablePprof:                         ctx.GlobalBool(flags.EnablePprof.Name),
		DisableDispersalAuthentication:      ctx.GlobalBool(flags.DisableDispersalAuthenticationFlag.Name),
		DispersalAuthenticationKeyCacheSize: ctx.GlobalInt(flags.DispersalAuthenticationKeyCacheSizeFlag.Name),
		DisperserKeyTimeout:                 ctx.GlobalDuration(flags.DisperserKeyTimeoutFlag.Name),
		DispersalAuthenticationTimeout:      ctx.GlobalDuration(flags.DispersalAuthenticationTimeoutFlag.Name),
	}, nil
}
