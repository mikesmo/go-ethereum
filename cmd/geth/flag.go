// Copyright 2015 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"crypto/ecdsa"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"errors"

	"github.com/ethereumproject/ethash"
	"github.com/ethereumproject/go-ethereum/accounts"
	"github.com/ethereumproject/go-ethereum/common"
	"github.com/ethereumproject/go-ethereum/core"
	"github.com/ethereumproject/go-ethereum/core/state"
	"github.com/ethereumproject/go-ethereum/core/types"
	"github.com/ethereumproject/go-ethereum/crypto"
	"github.com/ethereumproject/go-ethereum/eth"
	"github.com/ethereumproject/go-ethereum/ethdb"
	"github.com/ethereumproject/go-ethereum/event"
	"github.com/ethereumproject/go-ethereum/logger"
	"github.com/ethereumproject/go-ethereum/logger/glog"
	"github.com/ethereumproject/go-ethereum/miner"
	"github.com/ethereumproject/go-ethereum/node"
	"github.com/ethereumproject/go-ethereum/p2p/discover"
	"github.com/ethereumproject/go-ethereum/p2p/nat"
	"github.com/ethereumproject/go-ethereum/pow"
	"github.com/ethereumproject/go-ethereum/whisper"
	"gopkg.in/urfave/cli.v1"
)

func init() {
	cli.AppHelpTemplate = `{{.Name}} {{if .Flags}}[global options] {{end}}command{{if .Flags}} [command options]{{end}} [arguments...]

VERSION:
   {{.Version}}

COMMANDS:
   {{range .Commands}}{{.Name}}{{with .ShortName}}, {{.}}{{end}}{{ "\t" }}{{.Usage}}
   {{end}}{{if .Flags}}
GLOBAL OPTIONS:
   {{range .Flags}}{{.}}
   {{end}}{{end}}
`

	cli.CommandHelpTemplate = `{{.Name}}{{if .Subcommands}} command{{end}}{{if .Flags}} [command options]{{end}} [arguments...]
{{if .Description}}{{.Description}}
{{end}}{{if .Subcommands}}
SUBCOMMANDS:
	{{range .Subcommands}}{{.Name}}{{with .ShortName}}, {{.}}{{end}}{{ "\t" }}{{.Usage}}
	{{end}}{{end}}{{if .Flags}}
OPTIONS:
	{{range .Flags}}{{.}}
	{{end}}{{end}}
`
}

var (
	ErrInvalidFlag        = errors.New("invalid flag or context value")
	ErrInvalidChainID     = errors.New("invalid chainID")
	ErrDirectoryStructure = errors.New("error in directory structure")
	ErrStackFail          = errors.New("error in stack protocol")
)

var currentChainID string
var reservedChainIDS = map[string]bool{
	"chaindata": true,
	"dapp":      true,
	"keystore":  true,
	"nodekey":   true,
	"nodes":     true,
	"geth":      true,
}

var testnetChaidIDs = map[string]bool{
	"morden": true,
	"testnet": true,
}

// isTestMode checks if either '--testnet' or '--chain=morden|testnet' or {"id": "morden"/"testnet"} in use for context, checking flags and external config settings
func isTestMode(ctx *cli.Context) bool {
	return ctx.GlobalBool(aliasableName(TestNetFlag.Name, ctx)) || testnetChaidIDs[ctx.GlobalString(aliasableName(ChainIDFlag.Name, ctx))] || testnetChaidIDs[currentChainID]
}

func getChainIDFromFlag(ctx *cli.Context) string {
	if id := ctx.GlobalString(aliasableName(ChainIDFlag.Name, ctx)); id != "" {
		if reservedChainIDS[id] {
			glog.Fatalf(`%v: %v: reserved word
					reserved words for --chain flag include: 'chaindata', 'dapp', 'keystore', 'nodekey', 'nodes',
					please use a different identifier`, ErrInvalidFlag, ErrInvalidChainID)
			return ""
		}
		return id
	} else {
		glog.Fatalf("%v: %v: chainID empty", ErrInvalidFlag, ErrInvalidChainID)
		return ""
	}
}

// getChainConfigIDFromContext gets the --chain=my-custom-net value
// Disallowed words which conflict with in-use data files will log the problem and return "", which cause an error.
func getChainConfigIDFromContext(ctx *cli.Context) string {
	chainFlagIsSet := ctx.GlobalBool(aliasableName(TestNetFlag.Name, ctx)) || ctx.GlobalIsSet(aliasableName(ChainIDFlag.Name, ctx))

	// make external config (sets chainid) incompatible with overlapping --chainid flag
	if chainFlagIsSet && currentChainID != "" {
		glog.Fatalf("%v: %v: external and flag configurations are conflicting, please use only one",
			ErrInvalidFlag, ErrInvalidChainID)
	}
	// set from external config
	if currentChainID != "" {
		return currentChainID
	}
	if isTestMode(ctx) {
		// ie '--testnet --chain customtestnet'
		if ctx.GlobalIsSet(aliasableName(ChainIDFlag.Name, ctx)) && !testnetChaidIDs[ctx.GlobalString(aliasableName(ChainIDFlag.Name, ctx))] {
			return getChainIDFromFlag(ctx)
		}
		return core.DefaultTestnetChainConfigID // this makes '--testnet', '--chain=testnet', and '--chain=morden' all use the same /morden subdirectory, if --chain isn't specified
	}
	if ctx.GlobalIsSet(aliasableName(ChainIDFlag.Name, ctx)) {
		return getChainIDFromFlag(ctx)
	}
	return core.DefaultChainConfigID
}

// getChainConfigNameFromContext gets mainnet or testnet defaults if in use.
// _If a custom net is in use, it echoes the name of the ChainConfigID._
// It is intended to be a human-readable name for a chain configuration.
func getChainConfigNameFromContext(ctx *cli.Context) string {
	if isTestMode(ctx) {
		return core.DefaultTestnetChainConfigName
	}
	if ctx.GlobalIsSet(aliasableName(ChainIDFlag.Name, ctx)) {
		return getChainConfigIDFromContext(ctx) // shortcut
	}
	return core.DefaultChainConfigName
}

// mustMakeDataDir retrieves the currently requested data directory, terminating
// if none (or the empty string) is specified.
// --> <home>/<EthereumClassic>(defaulty) or --datadir
func mustMakeDataDir(ctx *cli.Context) string {
	if path := ctx.GlobalString(aliasableName(DataDirFlag.Name, ctx)); path != "" {
		return path
	}

	glog.Fatalf("%v: cannot determine data directory, please set manually (--%v)", ErrDirectoryStructure, DataDirFlag.Name)
	return ""
}

// MustMakeChainDataDir retrieves the currently requested data directory including chain-specific subdirectory.
// A subdir of the datadir is used for each chain configuration ("/mainnet", "/testnet", "/my-custom-net").
// --> <home>/<EthereumClassic>/<mainnet|testnet|custom-net>, per --chain
func MustMakeChainDataDir(ctx *cli.Context) string {
	return common.EnsureAbsolutePath(mustMakeDataDir(ctx), getChainConfigIDFromContext(ctx))
}

// MakeIPCPath creates an IPC path configuration from the set command line flags,
// returning an empty string if IPC was explicitly disabled, or the set path.
func MakeIPCPath(ctx *cli.Context) string {
	if ctx.GlobalBool(aliasableName(IPCDisabledFlag.Name, ctx)) {
		return ""
	}
	return ctx.GlobalString(aliasableName(IPCPathFlag.Name, ctx))
}

// MakeNodeKey creates a node key from set command line flags, either loading it
// from a file or as a specified hex value. If neither flags were provided, this
// method returns nil and an emphemeral key is to be generated.
func MakeNodeKey(ctx *cli.Context) *ecdsa.PrivateKey {
	var (
		hex  = ctx.GlobalString(aliasableName(NodeKeyHexFlag.Name, ctx))
		file = ctx.GlobalString(aliasableName(NodeKeyFileFlag.Name, ctx))

		key *ecdsa.PrivateKey
		err error
	)
	switch {
	case file != "" && hex != "":
		log.Fatalf("Options %q and %q are mutually exclusive", aliasableName(NodeKeyFileFlag.Name, ctx), aliasableName(NodeKeyHexFlag.Name, ctx))

	case file != "":
		if key, err = crypto.LoadECDSA(file); err != nil {
			log.Fatalf("Option %q: %v", aliasableName(NodeKeyFileFlag.Name, ctx), err)
		}

	case hex != "":
		if key, err = crypto.HexToECDSA(hex); err != nil {
			log.Fatalf("Option %q: %v", aliasableName(NodeKeyHexFlag.Name, ctx), err)
		}
	}
	return key
}

// MakeBootstrapNodesFromContext creates a list of bootstrap nodes from the command line
// flags, reverting to pre-configured ones if none have been specified.
func MakeBootstrapNodesFromContext(ctx *cli.Context) []*discover.Node {
	// Return pre-configured nodes if none were manually requested
	if !ctx.GlobalIsSet(aliasableName(BootnodesFlag.Name, ctx)) {

		// --testnet/--chain=morden flag overrides --config flag
		if isTestMode(ctx) {
			return TestNetBootNodes
		}
		return HomesteadBootNodes
	}
	return core.ParseBootstrapNodeStrings(strings.Split(ctx.GlobalString(aliasableName(BootnodesFlag.Name, ctx)), ","))
}

// MakeListenAddress creates a TCP listening address string from set command
// line flags.
func MakeListenAddress(ctx *cli.Context) string {
	return fmt.Sprintf(":%d", ctx.GlobalInt(aliasableName(ListenPortFlag.Name, ctx)))
}

// MakeNAT creates a port mapper from set command line flags.
func MakeNAT(ctx *cli.Context) nat.Interface {
	natif, err := nat.Parse(ctx.GlobalString(aliasableName(NATFlag.Name, ctx)))
	if err != nil {
		log.Fatalf("Option %s: %v", aliasableName(NATFlag.Name, ctx), err)
	}
	return natif
}

// MakeRPCModules splits input separated by a comma and trims excessive white
// space from the substrings.
func MakeRPCModules(input string) []string {
	result := strings.Split(input, ",")
	for i, r := range result {
		result[i] = strings.TrimSpace(r)
	}
	return result
}

// MakeHTTPRpcHost creates the HTTP RPC listener interface string from the set
// command line flags, returning empty if the HTTP endpoint is disabled.
func MakeHTTPRpcHost(ctx *cli.Context) string {
	if !ctx.GlobalBool(aliasableName(RPCEnabledFlag.Name, ctx)) {
		return ""
	}
	return ctx.GlobalString(aliasableName(RPCListenAddrFlag.Name, ctx))
}

// MakeWSRpcHost creates the WebSocket RPC listener interface string from the set
// command line flags, returning empty if the HTTP endpoint is disabled.
func MakeWSRpcHost(ctx *cli.Context) string {
	if !ctx.GlobalBool(aliasableName(WSEnabledFlag.Name, ctx)) {
		return ""
	}
	return ctx.GlobalString(aliasableName(WSListenAddrFlag.Name, ctx))
}

// MakeDatabaseHandles raises out the number of allowed file handles per process
// for Geth and returns half of the allowance to assign to the database.
func MakeDatabaseHandles() int {
	if err := raiseFdLimit(2048); err != nil {
		glog.V(logger.Warn).Info("Failed to raise file descriptor allowance: ", err)
	}
	limit, err := getFdLimit()
	if err != nil {
		glog.V(logger.Warn).Info("Failed to retrieve file descriptor allowance: ", err)
	}
	if limit > 2048 { // cap database file descriptors even if more is available
		limit = 2048
	}
	return limit / 2 // Leave half for networking and other stuff
}

// MakeAccountManager creates an account manager from set command line flags.
func MakeAccountManager(ctx *cli.Context) *accounts.Manager {
	// Create the keystore crypto primitive, light if requested
	scryptN := accounts.StandardScryptN
	scryptP := accounts.StandardScryptP
	if ctx.GlobalBool(aliasableName(LightKDFFlag.Name, ctx)) {
		scryptN = accounts.LightScryptN
		scryptP = accounts.LightScryptP
	}
	datadir := MustMakeChainDataDir(ctx)

	keydir := filepath.Join(datadir, "keystore")
	if path := ctx.GlobalString(aliasableName(KeyStoreDirFlag.Name, ctx)); path != "" {
		keydir = path
	}

	m, err := accounts.NewManager(keydir, scryptN, scryptP)
	if err != nil {
		glog.Fatalf("init account manager at %q: %s", keydir, err)
	}
	return m
}

// MakeAddress converts an account specified directly as a hex encoded string or
// a key index in the key store to an internal account representation.
func MakeAddress(accman *accounts.Manager, account string) (accounts.Account, error) {
	// If the specified account is a valid address, return it
	if common.IsHexAddress(account) {
		return accounts.Account{Address: common.HexToAddress(account)}, nil
	}
	// Otherwise try to interpret the account as a keystore index
	index, err := strconv.Atoi(account)
	if err != nil {
		return accounts.Account{}, fmt.Errorf("invalid account address or index: %q", account)
	}
	return accman.AccountByIndex(index)
}

// MakeEtherbase retrieves the etherbase either from the directly specified
// command line flags or from the keystore if CLI indexed.
func MakeEtherbase(accman *accounts.Manager, ctx *cli.Context) common.Address {
	accounts := accman.Accounts()
	if !ctx.GlobalIsSet(aliasableName(EtherbaseFlag.Name, ctx)) && len(accounts) == 0 {
		glog.V(logger.Error).Infoln("WARNING: No etherbase set and no accounts found as default")
		return common.Address{}
	}
	etherbase := ctx.GlobalString(aliasableName(EtherbaseFlag.Name, ctx))
	if etherbase == "" {
		return common.Address{}
	}
	// If the specified etherbase is a valid address, return it
	account, err := MakeAddress(accman, etherbase)
	if err != nil {
		log.Fatalf("Option %q: %v", aliasableName(EtherbaseFlag.Name, ctx), err)
	}
	return account.Address
}

// MakePasswordList reads password lines from the file specified by --password.
func MakePasswordList(ctx *cli.Context) []string {
	path := ctx.GlobalString(aliasableName(PasswordFileFlag.Name, ctx))
	if path == "" {
		return nil
	}
	text, err := ioutil.ReadFile(path)
	if err != nil {
		glog.Fatal("Failed to read password file: ", err)
	}
	lines := strings.Split(string(text), "\n")
	// Sanitise DOS line endings.
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	return lines
}

// migrateExistingDirToClassicNamingScheme renames default base data directory ".../Ethereum" to ".../EthereumClassic", pending os customs, etc... ;-)
///
// Check for preexisting **Un-classic** data directory, ie "/home/path/to/Ethereum".
// If it exists, check if the data therein belongs to Classic blockchain (ie not configged as "ETF"),
// and rename it to fit Classic naming convention ("/home/path/to/EthereumClassic") if that dir doesn't already exist.
// This case only applies to Default, ie when a user **doesn't** provide a custom --datadir flag;
// a user should be able to override a specified data dir if they want.
func migrateExistingDirToClassicNamingScheme(ctx *cli.Context) error {

	ethDataDirPath := common.DefaultUnclassicDataDir()
	etcDataDirPath := common.DefaultDataDir()

	// only if default <EthereumClassic>/ datadir doesn't already exist
	if _, err := os.Stat(etcDataDirPath); err == nil {
		// classic data dir already exists
		glog.V(logger.Debug).Infof("Using existing ETClassic data directory at: %v\n", etcDataDirPath)
		return nil
	}

	ethChainDBPath := filepath.Join(ethDataDirPath, "chaindata")
	if isTestMode(ctx) {
		ethChainDBPath = filepath.Join(ethDataDirPath, "testnet", "chaindata")
	}

	// only if ETHdatadir chaindb path DOES already exist, so return nil if it doesn't;
	// otherwise NewLDBDatabase will create an empty one there.
	// note that this uses the 'old' non-subdirectory way of holding default data.
	// it must be called before migrating to subdirectories
	// NOTE: Since ETH stores chaindata by default in Ethereum/geth/..., this path
	// will not exist if the existing data belongs to ETH, so it works as a valid check for us as well.
	if _, err := os.Stat(ethChainDBPath); os.IsNotExist(err) {
		glog.V(logger.Debug).Infof(`No existing default chaindata dir found at: %v
		  	Using default data directory at: %v`,
			ethChainDBPath, etcDataDirPath)
		return nil
	}

	foundCorrectLookingFiles := []string{}
	requiredFiles := []string{"LOG", "LOCK", "CURRENT"}
	for _, f := range requiredFiles {
		p := filepath.Join(ethChainDBPath, f)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			glog.V(logger.Debug).Infof(`No existing default file found at: %v
		  	Using default data directory at: %v`,
				p, etcDataDirPath)
		} else {
			foundCorrectLookingFiles = append(foundCorrectLookingFiles, f)
		}
	}
	hasRequiredFiles := len(requiredFiles) == len(foundCorrectLookingFiles)
	if !hasRequiredFiles {
		return nil
	}


	// check if there is existing etf blockchain data in unclassic default dir (ie /<home>/Ethereum)
	chainDB, err := ethdb.NewLDBDatabase(ethChainDBPath, 0, 0)
	if err != nil {
		glog.V(logger.Error).Info(`Failed to check blockchain compatibility for existing Ethereum chaindata database at: %v
		 	Using default data directory at: %v`,
			err, etcDataDirPath)
		return nil
	}

	defer chainDB.Close()


	// Only move if defaulty ETC (mainnet or testnet).
	// Get head block if testnet, fork block if mainnet.
	hh := core.GetHeadBlockHash(chainDB) // get last block in fork
	if ctx.GlobalBool(aliasableName(FastSyncFlag.Name, ctx)) {
		hh = core.GetHeadFastBlockHash(chainDB)
	}
	if hh.IsEmpty() {
		glog.V(logger.Debug).Info("There was no head block for the old database. It could be very young.")
	}

	hasRequiredForkIfSufficientHeight := true
	if !hh.IsEmpty() {
		// if head block < 1920000, then its compatible
		// if head block >= 1920000, then it must have a hash matching required hash

		// Use default configuration to check if known fork, if block 1920000 exists.
		// If block1920000 doesn't exist, given above checks for directory structure expectations,
		// I think it's safe to assume that the chaindata directory is just too 'young', where it hasn't
		// synced until block 1920000, and therefore can be migrated.
		conf := core.DefaultConfig
		if isTestMode(ctx) {
			conf = core.TestConfig
		}

		hf := conf.ForkByName("The DAO Hard Fork")
		if hf == nil || hf.Block == nil || new(big.Int).Cmp(hf.Block) == 0 || hf.RequiredHash.IsEmpty(){
			glog.V(logger.Debug).Info("DAO Hard Fork required hash not configured for database chain. Not migrating.")
			return nil
		}

		b := core.GetBlock(chainDB, hh)
		if b == nil {
			glog.V(logger.Debug).Info("There was a problem checking the head block of old-namespaced database. The head hash was: %v", hh.Hex())
			return nil
		}

		// if head block >= 1920000
		if b.Number().Cmp(hf.Block) >= 0 {
			// now, since we know that the height is bigger than the hardfork, we have to check that the db contains the required hardfork hash
			glog.V(logger.Debug).Infof("Existing head block in old data dir has sufficient height: %v", b.String())

			hasRequiredForkIfSufficientHeight = false
			bf := core.GetBlock(chainDB, hf.RequiredHash)
			// does not have required block by hash
			if bf != nil {
				glog.V(logger.Debug).Infof("Head block has sufficient height AND required hash: %v", b.String())
				hasRequiredForkIfSufficientHeight = true
			} else {
				glog.V(logger.Debug).Infof("Head block has sufficient height but not required hash: %v", b.String())
			}
		// head block < 1920000
		} else {
			glog.V(logger.Debug).Infof("Existing head block in old data dir has INSUFFICIENT height to differentiate ETC/ETF: %v", b.String())
		}
	}

	if hasRequiredForkIfSufficientHeight {
		// if any of the LOG, LOCK, or CURRENT files are missing from old chaindata/, don't migrate
		glog.V(logger.Info).Infof(`Found existing data directory named 'Ethereum' with default ETC chaindata.
		  	Moving it from: %v, to: %v
		  	To specify a different data directory use the '--datadir' flag.`,
			ethDataDirPath, etcDataDirPath)
		return os.Rename(ethDataDirPath, etcDataDirPath)
	}

	glog.V(logger.Debug).Infof(`Existing default Ethereum database at: %v isn't an Ethereum Classic default blockchain.
	  	Will not migrate.
	  	Using ETC chaindata database at: %v`,
		ethDataDirPath, etcDataDirPath)
	return nil
}

// migrateToChainSubdirIfNecessary migrates ".../EthereumClassic/nodes|chaindata|...|nodekey" --> ".../EthereumClassic/mainnet/nodes|chaindata|...|nodekey"
func migrateToChainSubdirIfNecessary(ctx *cli.Context) error {
	name := getChainConfigIDFromContext(ctx) // "mainnet", "morden", "custom"

	datapath := mustMakeDataDir(ctx) // ".../EthereumClassic/ | --datadir"

	subdirPath := MustMakeChainDataDir(ctx) // ie, <EthereumClassic>/mainnet

	// check if default subdir "mainnet" exits
	// NOTE: this assumes that if the migration has been run once, the "mainnet" dir will exist and will have necessary datum inside it
	subdirPathInfo, err := os.Stat(subdirPath)
	if err == nil {
		// dir already exists
		return nil
	}
	if subdirPathInfo != nil && !subdirPathInfo.IsDir() {
		return fmt.Errorf(`%v: found file named '%v' in EthereumClassic datadir,
			which conflicts with default chain directory naming convention: %v`, ErrDirectoryStructure, name, subdirPath)
	}

	// 3.3 testnet uses subdir '/testnet'
	if testnetChaidIDs[name] {
		exTestDir := filepath.Join(subdirPath, "../testnet")
		exTestDirInfo, e := os.Stat(exTestDir)
		if e != nil && os.IsNotExist(e) {
			return nil // ex testnet dir doesn't exist
		}
		if !exTestDirInfo.IsDir() {
			return nil // don't interfere with user *file* that won't be relevant for geth
		}
		return os.Rename(exTestDir, subdirPath) // /testnet -> /morden
	}

	// mkdir -p ".../mainnet"
	if err := os.MkdirAll(subdirPath, 0755); err != nil {
		return err
	}

	// move if existing (nodekey, dapp/, keystore/, chaindata/, nodes/) into new subdirectories
	for _, dir := range []string{"dapp", "keystore", "chaindata", "nodes"} {

		dirPath := filepath.Join(datapath, dir)

		dirInfo, e := os.Stat(dirPath)
		if e != nil && os.IsNotExist(e) {
			continue // dir doesn't exist
		}
		if !dirInfo.IsDir() {
			continue // don't interfere with user *file* that won't be relevant for geth
		}

		dirPathUnderSubdir := filepath.Join(subdirPath, dir)
		if err := os.Rename(dirPath, dirPathUnderSubdir); err != nil {
			return err
		}
	}

	// ensure nodekey exists and is file (loop lets us stay consistent in form here, an keep options open for easy other files to include)
	for _, file := range []string{"nodekey", "geth.ipc"} {
		filePath := filepath.Join(datapath, file)

		// ensure exists and is a file
		fileInfo, e := os.Stat(filePath)
		if e != nil && os.IsNotExist(e) {
			continue
		}
		if fileInfo.IsDir() {
			continue // don't interfere with user dirs that won't be relevant for geth
		}

		filePathUnderSubdir := filepath.Join(subdirPath, file)
		if err := os.Rename(filePath, filePathUnderSubdir); err != nil {
			return err
		}
	}
	return nil
}

// makeName makes the node name, which can be (in part) customized by the IdentityFlag
func makeNodeName(version string, ctx *cli.Context) string {
	name := fmt.Sprintf("Geth/%s/%s/%s", version, runtime.GOOS, runtime.Version())
	if identity := ctx.GlobalString(aliasableName(IdentityFlag.Name, ctx)); len(identity) > 0 {
		name += "/" + identity
	}
	return name
}

// iff the --chain flag is set AND it is not contained in [mainnet, testnet, OR morden]
func chainIdIsCustom(ctx *cli.Context) bool {
	// Chain id set from flag.
	if ctx.GlobalIsSet(aliasableName(ChainIDFlag.Name, ctx)) {
		n := ctx.GlobalString(aliasableName(ChainIDFlag.Name, ctx))
		return n != "mainnet" && !testnetChaidIDs[n]
	}
	// Chain id set from external file.
	if currentChainID != "" {
		return currentChainID != "mainnet" && !testnetChaidIDs[currentChainID]
	}
	return false
}

// MakeSystemNode sets up a local node, configures the services to launch and
// assembles the P2P protocol stack.
func MakeSystemNode(version string, ctx *cli.Context) *node.Node {

	name := makeNodeName(version, ctx)

	// global settings

	if ctx.GlobalIsSet(aliasableName(ExtraDataFlag.Name, ctx)) {
		s := ctx.GlobalString(aliasableName(ExtraDataFlag.Name, ctx))
		if len(s) > types.HeaderExtraMax {
			log.Fatalf("%s flag %q exceeds size limit of %d", aliasableName(ExtraDataFlag.Name, ctx), s, types.HeaderExtraMax)
		}
		miner.HeaderExtra = []byte(s)
	}

	// Data migrations...

	// Rename existing default datadir <home>/<Ethereum>/ to <home>/<EthereumClassic>.
	// Only do this if --datadir flag is not specified AND <home>/<EthereumClassic> does NOT already exist (only migrate once and only for defaulty).
	// If it finds an 'Ethereum' directory, it will check if it contains default ETC or ETHF chain data.
	// If it contains ETC data, it will rename the dir. If ETHF data, if will do nothing.
	if !ctx.GlobalIsSet(aliasableName(DataDirFlag.Name, ctx)) {
		if !chainIdIsCustom(ctx) {
			if !ctx.GlobalIsSet(aliasableName(UseChainConfigFlag.Name, ctx)) {
				if migrationError := migrateExistingDirToClassicNamingScheme(ctx); migrationError != nil {
					glog.Fatalf("%v: failed to migrate existing Classic database: %v", ErrDirectoryStructure, migrationError)
				}
			}

		}
	}
	// Move existing mainnet data to pertinent chain-named subdir scheme (ie ethereum-classic/mainnet).
	// This should only happen if the given (newly defined in this protocol) subdir doesn't exist,
	// and the dirs&files (nodekey, dapp, keystore, chaindata, nodes) do exist,
	if !ctx.GlobalIsSet(aliasableName(DataDirFlag.Name, ctx)) {
		if !chainIdIsCustom(ctx) {
			if !ctx.GlobalIsSet(aliasableName(UseChainConfigFlag.Name, ctx)) {
				if subdirMigrateErr := migrateToChainSubdirIfNecessary(ctx); subdirMigrateErr != nil {
					glog.Fatalf("%v: failed to migrate existing data to chain-specific subdir: %v", ErrDirectoryStructure, subdirMigrateErr)
				}
			}
		}
	}

	// Makes sufficient configuration from JSON file or DB pending flags.
	// Delegates flag usage.
	config := mustMakeSufficientConfiguration(ctx)

	// Avoid conflicting flags
	if ctx.GlobalBool(DevModeFlag.Name) && isTestMode(ctx) {
		glog.Fatalf("%v: flags --%v and --%v/--%v=morden are mutually exclusive", ErrInvalidFlag, DevModeFlag.Name, TestNetFlag.Name, ChainIDFlag.Name)
	}

	// Configure the node's service container
	stackConf := &node.Config{
		DataDir:         MustMakeChainDataDir(ctx),
		PrivateKey:      MakeNodeKey(ctx),
		Name:            name,
		NoDiscovery:     ctx.GlobalBool(aliasableName(NoDiscoverFlag.Name, ctx)),
		BootstrapNodes:  config.ParsedBootstrap,
		ListenAddr:      MakeListenAddress(ctx),
		NAT:             MakeNAT(ctx),
		MaxPeers:        ctx.GlobalInt(aliasableName(MaxPeersFlag.Name, ctx)),
		MaxPendingPeers: ctx.GlobalInt(aliasableName(MaxPendingPeersFlag.Name, ctx)),
		IPCPath:         MakeIPCPath(ctx),
		HTTPHost:        MakeHTTPRpcHost(ctx),
		HTTPPort:        ctx.GlobalInt(aliasableName(RPCPortFlag.Name, ctx)),
		HTTPCors:        ctx.GlobalString(aliasableName(RPCCORSDomainFlag.Name, ctx)),
		HTTPModules:     MakeRPCModules(ctx.GlobalString(aliasableName(RPCApiFlag.Name, ctx))),
		WSHost:          MakeWSRpcHost(ctx),
		WSPort:          ctx.GlobalInt(aliasableName(WSPortFlag.Name, ctx)),
		WSOrigins:       ctx.GlobalString(aliasableName(WSAllowedOriginsFlag.Name, ctx)),
		WSModules:       MakeRPCModules(ctx.GlobalString(aliasableName(WSApiFlag.Name, ctx))),
	}

	// Configure the Ethereum service
	accman := MakeAccountManager(ctx)

	ethConf := &eth.Config{
		ChainConfig:             config.ChainConfig,
		FastSync:                ctx.GlobalBool(aliasableName(FastSyncFlag.Name, ctx)),
		BlockChainVersion:       ctx.GlobalInt(aliasableName(BlockchainVersionFlag.Name, ctx)),
		DatabaseCache:           ctx.GlobalInt(aliasableName(CacheFlag.Name, ctx)),
		DatabaseHandles:         MakeDatabaseHandles(),
		NetworkId:               ctx.GlobalInt(aliasableName(NetworkIdFlag.Name, ctx)),
		AccountManager:          accman,
		Etherbase:               MakeEtherbase(accman, ctx),
		MinerThreads:            ctx.GlobalInt(aliasableName(MinerThreadsFlag.Name, ctx)),
		NatSpec:                 ctx.GlobalBool(aliasableName(NatspecEnabledFlag.Name, ctx)),
		DocRoot:                 ctx.GlobalString(aliasableName(DocRootFlag.Name, ctx)),
		GasPrice:                new(big.Int),
		GpoMinGasPrice:          new(big.Int),
		GpoMaxGasPrice:          new(big.Int),
		GpoFullBlockRatio:       ctx.GlobalInt(aliasableName(GpoFullBlockRatioFlag.Name, ctx)),
		GpobaseStepDown:         ctx.GlobalInt(aliasableName(GpobaseStepDownFlag.Name, ctx)),
		GpobaseStepUp:           ctx.GlobalInt(aliasableName(GpobaseStepUpFlag.Name, ctx)),
		GpobaseCorrectionFactor: ctx.GlobalInt(aliasableName(GpobaseCorrectionFactorFlag.Name, ctx)),
		SolcPath:                ctx.GlobalString(aliasableName(SolcPathFlag.Name, ctx)),
		AutoDAG:                 ctx.GlobalBool(aliasableName(AutoDAGFlag.Name, ctx)) || ctx.GlobalBool(aliasableName(MiningEnabledFlag.Name, ctx)),
	}

	if _, ok := ethConf.GasPrice.SetString(ctx.GlobalString(aliasableName(GasPriceFlag.Name, ctx)), 0); !ok {
		log.Fatalf("malformed %s flag value %q", aliasableName(GasPriceFlag.Name, ctx), ctx.GlobalString(aliasableName(GasPriceFlag.Name, ctx)))
	}
	if _, ok := ethConf.GpoMinGasPrice.SetString(ctx.GlobalString(aliasableName(GpoMinGasPriceFlag.Name, ctx)), 0); !ok {
		log.Fatalf("malformed %s flag value %q", aliasableName(GpoMinGasPriceFlag.Name, ctx), ctx.GlobalString(aliasableName(GpoMinGasPriceFlag.Name, ctx)))
	}
	if _, ok := ethConf.GpoMaxGasPrice.SetString(ctx.GlobalString(aliasableName(GpoMaxGasPriceFlag.Name, ctx)), 0); !ok {
		log.Fatalf("malformed %s flag value %q", aliasableName(GpoMaxGasPriceFlag.Name, ctx), ctx.GlobalString(aliasableName(GpoMaxGasPriceFlag.Name, ctx)))
	}

	// Configure the Whisper service
	shhEnable := ctx.GlobalBool(aliasableName(WhisperEnabledFlag.Name, ctx))

	if ctx.GlobalIsSet(aliasableName(UseChainConfigFlag.Name, ctx)) {
		ethConf.Genesis = config.Genesis // from parsed JSON file
	}

	// Override any default configs in dev mode or the test net
	switch {
	case isTestMode(ctx):
		if !ctx.GlobalIsSet(aliasableName(NetworkIdFlag.Name, ctx)) {
			ethConf.NetworkId = 2
		}
		ethConf.Genesis = core.TestNetGenesis
		state.StartingNonce = 1048576 // (2**20)

	case ctx.GlobalBool(aliasableName(DevModeFlag.Name, ctx)):
		// Override the base network stack configs
		if !ctx.GlobalIsSet(aliasableName(DataDirFlag.Name, ctx)) {
			stackConf.DataDir = filepath.Join(os.TempDir(), "/ethereum_dev_mode")
		}
		if !ctx.GlobalIsSet(aliasableName(MaxPeersFlag.Name, ctx)) {
			stackConf.MaxPeers = 0
		}
		if !ctx.GlobalIsSet(aliasableName(ListenPortFlag.Name, ctx)) {
			stackConf.ListenAddr = ":0"
		}
		// Override the Ethereum protocol configs
		ethConf.Genesis = core.TestNetGenesis
		if !ctx.GlobalIsSet(aliasableName(GasPriceFlag.Name, ctx)) {
			ethConf.GasPrice = new(big.Int)
		}
		if !ctx.GlobalIsSet(aliasableName(WhisperEnabledFlag.Name, ctx)) {
			shhEnable = true
		}
		ethConf.PowTest = true
	}
	// Assemble and return the protocol stack
	stack, err := node.New(stackConf)
	if err != nil {
		glog.Fatalf("%v: failed to create the protocol stack: ", ErrStackFail, err)
	}
	if err := stack.Register(func(ctx *node.ServiceContext) (node.Service, error) {
		return eth.New(ctx, ethConf)
	}); err != nil {
		glog.Fatalf("%v: failed to register the Ethereum service: ", ErrStackFail, err)
	}
	if shhEnable {
		if err := stack.Register(func(*node.ServiceContext) (node.Service, error) { return whisper.New(), nil }); err != nil {
			glog.Fatalf("%v: failed to register the Whisper service: ", ErrStackFail, err)
		}
	}

	if ctx.GlobalBool(Unused1.Name) {
		glog.V(logger.Info).Infoln(fmt.Sprintf("Geth started with --%s flag, which is unused by Geth Classic and can be omitted", Unused1.Name))
	}

	return stack
}

// MustMakeChainConfig reads the chain configuration from the database in ctx.Datadir.
func MustMakeChainConfig(ctx *cli.Context) *core.ChainConfig {

	db := MakeChainDatabase(ctx)
	defer db.Close()
	glog.V(logger.Info).Info(fmt.Sprintf("Did read chain configuration from database: \x1b[32m%s\x1b[39m", MustMakeChainDataDir(ctx)+"/chaindata"))

	return MustMakeChainConfigFromDb(ctx, db)
}

// mustMakeSufficientConfiguration makes a sufficent chain configuration (id, chainconfig, nodes,...) from
// either JSON file path, DB, or fails hard.
// Forces users to provide a full and complete config file if any is specified.
// Delegates flags to determine which source to use for configuration setup.
func mustMakeSufficientConfiguration(ctx *cli.Context) *core.SufficientChainConfig {

	config := &core.SufficientChainConfig{}

	// If external JSON --chainconfig specified.
	if ctx.GlobalIsSet(aliasableName(UseChainConfigFlag.Name, ctx)) {
		glog.V(logger.Info).Info(fmt.Sprintf("Using custom chain configuration file: \x1b[32m%s\x1b[39m",
			filepath.Clean(ctx.GlobalString(aliasableName(UseChainConfigFlag.Name, ctx)))))

		// Returns surely valid suff chain config.
		config, err := core.ReadExternalChainConfig(ctx.GlobalString(aliasableName(UseChainConfigFlag.Name, ctx)))
		if err != nil {
			glog.Fatalf("invalid external configuration JSON: %v", err)
		}

		// Check for flagged bootnodes.
		if ctx.GlobalIsSet(aliasableName(BootnodesFlag.Name, ctx)) {
			panic("Conflicting --chain-config and --bootnodes flags. Please use either but not both.")
		}

		currentChainID = config.ID // Set global var.
		logChainConfiguration(ctx, config)

		return config
	}

	// Initialise chain configuration before handling migrations or setting up node.
	config.ID = getChainConfigIDFromContext(ctx)
	config.Name = getChainConfigNameFromContext(ctx)
	config.ChainConfig = MustMakeChainConfig(ctx).SortForks()
	config.ParsedBootstrap = MakeBootstrapNodesFromContext(ctx)
	logChainConfiguration(ctx, config)

	return config
}

func logChainConfiguration(ctx *cli.Context, config *core.SufficientChainConfig) {

	glog.V(logger.Info).Info(glog.Separator("-"))

	glog.V(logger.Info).Info(fmt.Sprintf("Starting Geth Classic \x1b[32m%s\x1b[39m", ctx.App.Version))
	glog.V(logger.Info).Infof("Geth is configured to use blockchain: \x1b[32m%v (ETC)\x1b[39m", config.Name)

	glog.V(logger.Info).Info(fmt.Sprintf("%v blockchain upgrades associated with this configuration:", len(config.ChainConfig.Forks)))

	for i := range config.ChainConfig.Forks {
		glog.V(logger.Info).Info(fmt.Sprintf(" %7v %v", config.ChainConfig.Forks[i].Block, config.ChainConfig.Forks[i].Name))
		if !config.ChainConfig.Forks[i].RequiredHash.IsEmpty() {
			glog.V(logger.Info).Info(fmt.Sprintf("         with block %v", config.ChainConfig.Forks[i].RequiredHash.Hex()))
		}
		for _, feat := range config.ChainConfig.Forks[i].Features {
			glog.V(logger.Debug).Infof("    id: %v", feat.ID)
			for k, v := range feat.Options {
				glog.V(logger.Debug).Infof("        %v: %v", k, v)
			}
		}
	}

	glog.V(logger.Info).Info(glog.Separator("-"))
}

// MustMakeChainConfigFromDb reads the chain configuration from the given database.
func MustMakeChainConfigFromDb(ctx *cli.Context, db ethdb.Database) *core.ChainConfig {

	c := core.DefaultConfig
	configName := "mainnet"
	if isTestMode(ctx) {
		c = core.TestConfig
		configName = "morden testnet"
	}

	glog.V(logger.Info).Info(fmt.Sprintf("Loading \x1b[36m%v\x1b[39m configuration from database...", configName))

	return c
}

// MakeChainDatabase open an LevelDB using the flags passed to the client and will hard crash if it fails.
func MakeChainDatabase(ctx *cli.Context) ethdb.Database {
	var (
		datadir = MustMakeChainDataDir(ctx)
		cache   = ctx.GlobalInt(aliasableName(CacheFlag.Name, ctx))
		handles = MakeDatabaseHandles()
	)

	chainDb, err := ethdb.NewLDBDatabase(filepath.Join(datadir, "chaindata"), cache, handles)
	if err != nil {
		glog.Fatal("Could not open database: ", err)
	}
	return chainDb
}

// MakeChain creates a chain manager from set command line flags.
func MakeChain(ctx *cli.Context) (chain *core.BlockChain, chainDb ethdb.Database) {
	var err error
	chainDb = MakeChainDatabase(ctx)

	chainConfig := MustMakeChainConfigFromDb(ctx, chainDb)

	pow := pow.PoW(core.FakePow{})
	if !ctx.GlobalBool(aliasableName(FakePoWFlag.Name, ctx)) {
		pow = ethash.New()
	}
	chain, err = core.NewBlockChain(chainDb, chainConfig, pow, new(event.TypeMux))
	if err != nil {
		glog.Fatal("Could not start chainmanager: ", err)
	}
	return chain, chainDb
}

// MakeConsolePreloads retrieves the absolute paths for the console JavaScript
// scripts to preload before starting.
func MakeConsolePreloads(ctx *cli.Context) []string {
	// Skip preloading if there's nothing to preload
	if ctx.GlobalString(aliasableName(PreloadJSFlag.Name, ctx)) == "" {
		return nil
	}
	// Otherwise resolve absolute paths and return them
	preloads := []string{}

	assets := ctx.GlobalString(aliasableName(JSpathFlag.Name, ctx))
	for _, file := range strings.Split(ctx.GlobalString(aliasableName(PreloadJSFlag.Name, ctx)), ",") {
		preloads = append(preloads, common.EnsureAbsolutePath(assets, strings.TrimSpace(file)))
	}
	return preloads
}
