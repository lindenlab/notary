package main

import (
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"errors"
	_ "expvar"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	_ "github.com/docker/distribution/health"
	"github.com/docker/notary/cryptoservice"
	"github.com/docker/notary/signer"
	"github.com/docker/notary/signer/api"
	"github.com/docker/notary/version"
	"github.com/endophage/gotuf/data"
	_ "github.com/go-sql-driver/mysql"
	"github.com/miekg/pkcs11"

	"github.com/Sirupsen/logrus"
	pb "github.com/docker/notary/proto"
	"github.com/spf13/viper"
)

const (
	debugAddr       = "localhost:8082"
	dbType          = "mysql"
	envPrefix       = "NOTARY_SIGNER"
	defaultAliasEnv = "DEFAULT_ALIAS"
	pinCode         = "PIN"
)

var debug bool
var configFile string

func init() {
	// set default log level to Error
	viper.SetDefault("logging", map[string]interface{}{"level": 2})

	viper.SetEnvPrefix(envPrefix)
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	// Setup flags
	flag.StringVar(&configFile, "config", "", "Path to configuration file")
	flag.BoolVar(&debug, "debug", false, "show the version and exit")
}

func passphraseRetriever(keyName, alias string, createNew bool, attempts int) (passphrase string, giveup bool, err error) {
	passphrase = viper.GetString(strings.ToUpper(alias))

	if passphrase == "" {
		return "", false, errors.New("expected env variable to not be empty: " + alias)
	}

	return passphrase, false, nil
}

func main() {
	flag.Usage = usage
	flag.Parse()

	if debug {
		go debugServer(debugAddr)
	}

	// when the signer starts print the version for debugging and issue logs later
	logrus.Infof("Version: %s, Git commit: %s", version.NotaryVersion, version.GitCommit)

	filename := filepath.Base(configFile)
	ext := filepath.Ext(configFile)
	configPath := filepath.Dir(configFile)

	viper.SetConfigType(strings.TrimPrefix(ext, "."))
	viper.SetConfigName(strings.TrimSuffix(filename, ext))
	viper.AddConfigPath(configPath)
	err := viper.ReadInConfig()
	if err != nil {
		logrus.Error("Viper Error: ", err.Error())
		logrus.Error("Could not read config at ", configFile)
		os.Exit(1)
	}

	logrus.SetLevel(logrus.Level(viper.GetInt("logging.level")))

	certFile := viper.GetString("server.cert_file")
	keyFile := viper.GetString("server.key_file")
	if certFile == "" || keyFile == "" {
		usage()
		log.Fatalf("Certificate and key are mandatory")
	}

	tlsConfig := &tls.Config{
		MinVersion:               tls.VersionTLS12,
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA},
	}
	tlsConfig.Rand = rand.Reader

	cryptoServices := make(signer.CryptoServiceIndex)

	pin := viper.GetString(pinCode)
	pkcs11Lib := viper.GetString("crypto.pkcs11lib")
	if pkcs11Lib != "" {
		if pin == "" {
			log.Fatalf("Using PIN is mandatory with pkcs11")
		}

		ctx, session := SetupHSMEnv(pkcs11Lib, pin)

		defer cleanup(ctx, session)

		cryptoServices[data.RSAKey] = api.NewRSAHardwareCryptoService(ctx, session)
	}

	configDBType := strings.ToLower(viper.GetString("storage.backend"))
	dbURL := viper.GetString("storage.db_url")
	if configDBType != dbType || dbURL == "" {
		usage()
		log.Fatalf("Currently only a MySQL database backend is supported.")
	}
	dbSQL, err := sql.Open(configDBType, dbURL)
	if err != nil {
		log.Fatalf("failed to open the database: %s, %v", dbURL, err)
	}

	defaultAlias := viper.GetString(defaultAliasEnv)
	logrus.Debug("Default Alias: ", defaultAlias)
	keyStore, err := signer.NewKeyDBStore(passphraseRetriever, defaultAlias, configDBType, dbSQL)
	if err != nil {
		log.Fatalf("failed to create a new keydbstore: %v", err)
	}
	cryptoService := cryptoservice.NewCryptoService("", keyStore)

	cryptoServices[data.ED25519Key] = cryptoService
	cryptoServices[data.ECDSAKey] = cryptoService

	//RPC server setup
	kms := &api.KeyManagementServer{CryptoServices: cryptoServices}
	ss := &api.SignerServer{CryptoServices: cryptoServices}

	grpcServer := grpc.NewServer()
	pb.RegisterKeyManagementServer(grpcServer, kms)
	pb.RegisterSignerServer(grpcServer, ss)

	rpcAddr := viper.GetString("server.grpc_addr")
	lis, err := net.Listen("tcp", rpcAddr)
	if err != nil {
		log.Fatalf("failed to listen %v", err)
	}
	creds, err := credentials.NewServerTLSFromFile(certFile, keyFile)
	if err != nil {
		log.Fatalf("failed to generate credentials %v", err)
	}
	go grpcServer.Serve(creds.NewListener(lis))

	httpAddr := viper.GetString("server.http_addr")
	if httpAddr == "" {
		log.Fatalf("Server address is required")
	}
	//HTTP server setup
	server := http.Server{
		Addr:      httpAddr,
		Handler:   api.Handlers(cryptoServices),
		TLSConfig: tlsConfig,
	}

	if debug {
		log.Println("RPC server listening on", rpcAddr)
		log.Println("HTTP server listening on", httpAddr)
	}

	err = server.ListenAndServeTLS(certFile, keyFile)
	if err != nil {
		log.Fatal("HTTP server failed to start:", err)
	}
}

func usage() {
	log.Println("usage:", os.Args[0], "<config>")
	flag.PrintDefaults()
}

// debugServer starts the debug server with pprof, expvar among other
// endpoints. The addr should not be exposed externally. For most of these to
// work, tls cannot be enabled on the endpoint, so it is generally separate.
func debugServer(addr string) {
	log.Println("Debug server listening on", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("error listening on debug interface: %v", err)
	}
}

// SetupHSMEnv is a method that depends on the existences
func SetupHSMEnv(libraryPath, pin string) (*pkcs11.Ctx, pkcs11.SessionHandle) {
	p := pkcs11.New(libraryPath)

	if p == nil {
		log.Fatalf("Failed to init library")
	}

	if err := p.Initialize(); err != nil {
		log.Fatalf("Initialize error %s\n", err.Error())
	}

	slots, err := p.GetSlotList(true)
	if err != nil {
		log.Fatalf("Failed to list HSM slots %s", err)
	}
	// Check to see if we got any slots from the HSM.
	if len(slots) < 1 {
		log.Fatalln("No HSM Slots found")
	}

	// CKF_SERIAL_SESSION: TRUE if cryptographic functions are performed in serial with the application; FALSE if the functions may be performed in parallel with the application.
	// CKF_RW_SESSION: TRUE if the session is read/write; FALSE if the session is read-only
	session, err := p.OpenSession(slots[0], pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
	if err != nil {
		log.Fatalf("Failed to Start Session with HSM %s", err)
	}

	if err = p.Login(session, pkcs11.CKU_USER, pin); err != nil {
		log.Fatalf("User PIN %s\n", err.Error())
	}

	return p, session
}

func cleanup(ctx *pkcs11.Ctx, session pkcs11.SessionHandle) {
	ctx.Destroy()
	ctx.Finalize()
	ctx.CloseSession(session)
	ctx.Logout(session)
}
