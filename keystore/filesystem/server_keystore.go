package filesystem

import (
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/cossacklabs/acra/keystore"
	"github.com/cossacklabs/acra/utils"
	"github.com/cossacklabs/acra/zone"
	"github.com/cossacklabs/themis/gothemis/keys"
	log "github.com/sirupsen/logrus"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// FilesystemKeyStore represents keystore that reads keys from key folders, and stores them in memory.
type FilesystemKeyStore struct {
	keys                map[string][]byte
	privateKeyDirectory string
	publicKeyDirectory  string
	directory           string
	lock                *sync.RWMutex
	encryptor           keystore.KeyEncryptor
}

// NewFilesystemKeyStore creates new FilesystemKeyStore using same key folder for private and public keys.
func NewFilesystemKeyStore(directory string, encryptor keystore.KeyEncryptor) (*FilesystemKeyStore, error) {
	return NewFilesystemKeyStoreTwoPath(directory, directory, encryptor)
}

// NewFilesystemKeyStoreTwoPath creates new FilesystemKeyStore using separate folders for private and public keys.
func NewFilesystemKeyStoreTwoPath(privateKeyFolder, publicKeyFolder string, encryptor keystore.KeyEncryptor) (*FilesystemKeyStore, error) {
	// check folder for private key
	directory, err := utils.AbsPath(privateKeyFolder)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(directory)
	if nil == err && runtime.GOOS == "linux" && fi.Mode().Perm().String() != "-rwx------" {
		log.Errorln(" key store folder has an incorrect permissions")
		return nil, errors.New("key store folder has an incorrect permissions")
	}
	if privateKeyFolder != publicKeyFolder {
		// check folder for public key
		directory, err = utils.AbsPath(privateKeyFolder)
		if err != nil {
			return nil, err
		}
		fi, err = os.Stat(directory)
		if nil != err && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return &FilesystemKeyStore{privateKeyDirectory: privateKeyFolder, publicKeyDirectory: publicKeyFolder,
		keys: make(map[string][]byte), lock: &sync.RWMutex{}, encryptor: encryptor}, nil
}

func (store *FilesystemKeyStore) generateKeyPair(filename string, clientID []byte) (*keys.Keypair, error) {
	keypair, err := keys.New(keys.KEYTYPE_EC)
	if err != nil {
		return nil, err
	}
	privateKeysFolder := filepath.Dir(store.getPrivateKeyFilePath(filename))
	err = os.MkdirAll(privateKeysFolder, 0700)
	if err != nil {
		return nil, err
	}

	publicKeysFolder := filepath.Dir(store.getPublicKeyFilePath(filename))
	err = os.MkdirAll(publicKeysFolder, 0700)
	if err != nil {
		return nil, err
	}

	encryptedPrivate, err := store.encryptor.Encrypt(keypair.Private.Value, clientID)
	if err != nil {
		return nil, err
	}
	err = ioutil.WriteFile(store.getPrivateKeyFilePath(filename), encryptedPrivate, 0600)
	if err != nil {
		return nil, err
	}
	err = ioutil.WriteFile(store.getPublicKeyFilePath(fmt.Sprintf("%s.pub", filename)), keypair.Public.Value, 0644)
	if err != nil {
		return nil, err
	}
	return keypair, nil
}

func (store *FilesystemKeyStore) generateKey(filename string, length uint8) ([]byte, error) {
	randomBytes := make([]byte, length)
	_, err := rand.Read(randomBytes)
	// Note that err == nil only if we read len(b) bytes.
	if err != nil {
		log.Error(err)
		return nil, err
	}
	dirpath := filepath.Dir(store.getPrivateKeyFilePath(filename))
	err = os.MkdirAll(dirpath, 0700)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	err = ioutil.WriteFile(store.getPrivateKeyFilePath(filename), randomBytes, 0600)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	return randomBytes, nil
}

// GenerateZoneKey generates zone ID and zone key pair, encrypts private key using zoneID as context,
// and saves encrypted PK in the filem returns zoneID and public key.
// Returns error if generation or encryption fail.
func (store *FilesystemKeyStore) GenerateZoneKey() ([]byte, []byte, error) {
	/* save private key in fs, return id and public key*/
	var id []byte
	for {
		// generate until key not exists
		id = zone.GenerateZoneID()
		if !store.HasZonePrivateKey(id) {
			break
		}
	}

	keypair, err := store.generateKeyPair(getZoneKeyFilename(id), id)
	if err != nil {
		return []byte{}, []byte{}, err
	}
	store.lock.Lock()
	defer store.lock.Unlock()
	encryptedKey, err := store.encryptor.Encrypt(keypair.Private.Value, id)
	if err != nil {
		return nil, nil, nil
	}
	utils.FillSlice(byte(0), keypair.Private.Value)
	// cache key
	store.keys[getZoneKeyFilename(id)] = encryptedKey
	return id, keypair.Public.Value, nil
}

func (store *FilesystemKeyStore) getPrivateKeyFilePath(filename string) string {
	return fmt.Sprintf("%s%s%s", store.privateKeyDirectory, string(os.PathSeparator), filename)
}

func (store *FilesystemKeyStore) getPublicKeyFilePath(filename string) string {
	return fmt.Sprintf("%s%s%s", store.publicKeyDirectory, string(os.PathSeparator), filename)
}

func (store *FilesystemKeyStore) getPrivateKeyByFilename(id []byte, filename string) (*keys.PrivateKey, error) {
	if !keystore.ValidateID(id) {
		return nil, keystore.ErrInvalidClientID
	}
	store.lock.Lock()
	defer store.lock.Unlock()
	encryptedKey, ok := store.keys[filename]
	if !ok {
		encryptedPrivateKey, err := utils.LoadPrivateKey(store.getPrivateKeyFilePath(filename))
		if err != nil {
			return nil, err
		}
		encryptedKey = encryptedPrivateKey.Value
	}

	decryptedKey, err := store.encryptor.Decrypt(encryptedKey, id)
	if err != nil {
		return nil, err
	}
	log.Debugf("load key from fs: %s", filename)
	store.keys[filename] = encryptedKey
	return &keys.PrivateKey{Value: decryptedKey}, nil
}

// GetZonePrivateKey reads encrypted zone private key from fs, decrypts it with master key and zoneId
// and returns plaintext private key, or reading/decryption error.
func (store *FilesystemKeyStore) GetZonePrivateKey(id []byte) (*keys.PrivateKey, error) {
	fname := getZoneKeyFilename(id)
	return store.getPrivateKeyByFilename(id, fname)
}

// HasZonePrivateKey returns if private key for this zoneID exists in cache or is written to fs.
func (store *FilesystemKeyStore) HasZonePrivateKey(id []byte) bool {
	if !keystore.ValidateID(id) {
		return false
	}
	// add caching false answers. now if key doesn't exists than always checks on fs
	// it's system call and slow.
	if len(id) == 0 {
		return false
	}
	fname := getZoneKeyFilename(id)
	store.lock.RLock()
	defer store.lock.RUnlock()
	_, ok := store.keys[fname]
	if ok {
		return true
	}
	exists, _ := utils.FileExists(store.getPrivateKeyFilePath(fname))
	return exists
}

// GetPeerPublicKey returns public key for this clientID, gets it from cache or reads from fs.
func (store *FilesystemKeyStore) GetPeerPublicKey(id []byte) (*keys.PublicKey, error) {
	if !keystore.ValidateID(id) {
		return nil, keystore.ErrInvalidClientID
	}
	fname := getPublicKeyFilename(id)
	store.lock.Lock()
	defer store.lock.Unlock()
	key, ok := store.keys[fname]
	if ok {
		log.Debugf("load cached key: %s", fname)
		return &keys.PublicKey{Value: key}, nil
	}
	publicKey, err := utils.LoadPublicKey(store.getPublicKeyFilePath(fname))
	if err != nil {
		return nil, err
	}
	log.Debugf("load key from fs: %s", fname)
	store.keys[fname] = publicKey.Value
	return publicKey, nil
}

// GetPrivateKey reads encrypted client private key from fs, decrypts it with master key and clientID,
// and returns plaintext private key, or reading/decryption error.
func (store *FilesystemKeyStore) GetPrivateKey(id []byte) (*keys.PrivateKey, error) {
	fname := getServerKeyFilename(id)
	return store.getPrivateKeyByFilename(id, fname)
}

// GetServerDecryptionPrivateKey reads encrypted server storage private key from fs,
// decrypts it with master key and clientID,
// and returns plaintext private key, or reading/decryption error.
func (store *FilesystemKeyStore) GetServerDecryptionPrivateKey(id []byte) (*keys.PrivateKey, error) {
	fname := getServerDecryptionKeyFilename(id)
	return store.getPrivateKeyByFilename(id, fname)
}

// GenerateConnectorKeys generates AcraConnector transport EC keypair using clientID as part of key name.
// Writes encrypted private key and plaintext public key to fs.
// Returns error if writing/encryption failed.
func (store *FilesystemKeyStore) GenerateConnectorKeys(id []byte) error {
	if !keystore.ValidateID(id) {
		return keystore.ErrInvalidClientID
	}
	filename := getConnectorKeyFilename(id)

	_, err := store.generateKeyPair(filename, id)
	if err != nil {
		return err
	}
	return nil
}

// GenerateServerKeys generates AcraServer transport EC keypair using clientID as part of key name.
// Writes encrypted private key and plaintext public key to fs.
// Returns error if writing/encryption failed.
func (store *FilesystemKeyStore) GenerateServerKeys(id []byte) error {
	if !keystore.ValidateID(id) {
		return keystore.ErrInvalidClientID
	}
	filename := getServerKeyFilename(id)
	_, err := store.generateKeyPair(filename, id)
	if err != nil {
		return err
	}
	return nil
}

// GenerateTranslatorKeys generates AcraTranslator transport EC keypair using clientID as part of key name.
// Writes encrypted private key and plaintext public key to fs.
// Returns error if writing/encryption failed.
func (store *FilesystemKeyStore) GenerateTranslatorKeys(id []byte) error {
	if !keystore.ValidateID(id) {
		return keystore.ErrInvalidClientID
	}
	filename := getTranslatorKeyFilename(id)
	_, err := store.generateKeyPair(filename, id)
	if err != nil {
		return err
	}
	return nil
}

// GenerateDataEncryptionKeys generates Storage EC keypair for encrypting/decrypting data
// using clientID as part of key name.
// Writes encrypted private key and plaintext public key to fs.
// Returns error if writing/encryption failed.
func (store *FilesystemKeyStore) GenerateDataEncryptionKeys(id []byte) error {
	if !keystore.ValidateID(id) {
		return keystore.ErrInvalidClientID
	}
	_, err := store.generateKeyPair(getServerDecryptionKeyFilename(id), id)
	if err != nil {
		return err
	}
	return nil
}

// Reset clears all cached keys
func (store *FilesystemKeyStore) Reset() {
	for _, encryptedKey := range store.keys {
		utils.FillSlice(byte(0), encryptedKey)
	}
	store.keys = make(map[string][]byte)
}

// GetPoisonKeyPair generates EC keypair for encrypting/decrypting poison records, and writes it to fs
// encrypting private key or reads existing keypair from fs.
// Returns keypair or error if generation/decryption failed.
func (store *FilesystemKeyStore) GetPoisonKeyPair() (*keys.Keypair, error) {
	privatePath := store.getPrivateKeyFilePath(POISON_KEY_FILENAME)
	publicPath := store.getPublicKeyFilePath(fmt.Sprintf("%s.pub", POISON_KEY_FILENAME))
	privateExists, err := utils.FileExists(privatePath)
	if err != nil {
		return nil, err
	}
	publicExists, err := utils.FileExists(publicPath)
	if err != nil {
		return nil, err
	}
	if privateExists && publicExists {
		private, err := utils.LoadPrivateKey(privatePath)
		if err != nil {
			return nil, err
		}
		if private.Value, err = store.encryptor.Decrypt(private.Value, []byte(POISON_KEY_FILENAME)); err != nil {
			return nil, err
		}
		public, err := utils.LoadPublicKey(publicPath)
		if err != nil {
			return nil, err
		}
		return &keys.Keypair{Public: public, Private: private}, nil
	}
	log.Infoln("Generate poison key pair")
	return store.generateKeyPair(POISON_KEY_FILENAME, []byte(POISON_KEY_FILENAME))
}

// GetAuthKey generates basic auth key for acraWebconfig, and writes it encrypted to fs,
// or reads existing key from fs.
// Returns key or error of generation/decryption failed.
func (store *FilesystemKeyStore) GetAuthKey(remove bool) ([]byte, error) {
	keyPath := store.getPrivateKeyFilePath(BASIC_AUTH_KEY_FILENAME)
	keyExists, err := utils.FileExists(keyPath)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	if keyExists && !remove {
		key, err := utils.ReadFile(keyPath)
		if err != nil {
			log.Error(err)
			return nil, err
		}
		return key, nil
	}
	log.Infof("Generate basic auth key for AcraWebconfig to %v", keyPath)
	return store.generateKey(BASIC_AUTH_KEY_FILENAME, keystore.BASIC_AUTH_KEY_LENGTH)
}