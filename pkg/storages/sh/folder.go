package sh

import (
	"bufio"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/pkg/errors"
	"github.com/pkg/sftp"
	"github.com/wal-g/tracelog"
	"github.com/wal-g/wal-g/internal/contextio"
	"github.com/wal-g/wal-g/pkg/storages/storage"
	"golang.org/x/crypto/ssh"
)

type Folder struct {
	clientLazy func() (SftpClient, error)
	host       string
	path       string
	user       string
}

const (
	Port              = "SSH_PORT"
	Password          = "SSH_PASSWORD"
	Username          = "SSH_USERNAME"
	PrivateKeyPath    = "SSH_PRIVATE_KEY_PATH"
	defaultBufferSize = 64 * 1024 * 1024
)

var SettingsList = []string{
	Port,
	Password,
	Username,
	PrivateKeyPath,
}

func NewFolderError(err error, format string, args ...interface{}) storage.Error {
	return storage.NewError(err, "SSH", format, args...)
}

func ConfigureFolder(prefix string, settings map[string]string) (storage.HashableFolder, error) {
	host, folderPath, err := storage.ParsePrefixAsURL(prefix)

	if err != nil {
		return nil, err
	}

	user := settings[Username]
	pass := settings[Password]
	port := settings[Port]
	pkeyPath := settings[PrivateKeyPath]

	if port == "" {
		port = "22"
	}

	authMethods := []ssh.AuthMethod{}
	if pkeyPath != "" {
		pkey, err := os.ReadFile(pkeyPath)
		if err != nil {
			return nil, NewFolderError(err, "Unable to read private key: %v", err)
		}

		signer, err := ssh.ParsePrivateKey(pkey)
		if err != nil {
			return nil, NewFolderError(err, "Unable to parse private key: %v", err)
		}

		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if pass != "" {
		authMethods = append(authMethods, ssh.Password(pass))
	}

	config := &ssh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	address := fmt.Sprint(host, ":", port)
	clientLazy := makeClientLazy(address, config)

	folderPath = storage.AddDelimiterToPath(folderPath)

	return NewFolder(
		clientLazy,
		host,
		folderPath,
		user,
	), nil
}

func makeClientLazy(address string, config *ssh.ClientConfig) func() (SftpClient, error) {
	var connErr error
	var client SftpClient
	connOnce := new(sync.Once)
	return func() (SftpClient, error) {
		connOnce.Do(func() {
			sshClient, err := ssh.Dial("tcp", address, config)
			if err != nil {
				connErr = fmt.Errorf("failed to connect to %s via ssh", address)
				return
			}

			sftpClient, err := sftp.NewClient(sshClient)
			if err != nil {
				connErr = fmt.Errorf("failed to connect to %s via sftp", address)
				return
			}
			client = extend(sftpClient)
		})
		return client, connErr
	}
}

func NewFolder(clientLazy func() (SftpClient, error), host, path, user string) *Folder {
	return &Folder{
		clientLazy: clientLazy,
		host:       host,
		path:       path,
		user:       user,
	}
}

// TODO close ssh and sftp connection
// nolint: unused
func closeConnection(client io.Closer) {
	err := client.Close()
	if err != nil {
		tracelog.WarningLogger.FatalOnError(err)
	}
}

func (folder *Folder) GetPath() string {
	return folder.path
}

func (folder *Folder) ListFolder() (objects []storage.Object, subFolders []storage.Folder, err error) {
	client, err := folder.clientLazy()
	if err != nil {
		return nil, nil, err
	}

	filesInfo, err := client.ReadDir(folder.path)

	if os.IsNotExist(err) {
		// Folder does not exist, it means where are no objects in folder
		tracelog.DebugLogger.Println("\tskipped " + folder.path + ": " + err.Error())
		err = nil
		return
	}

	if err != nil {
		return nil, nil,
			NewFolderError(err, "Fail read folder '%s'", folder.path)
	}

	for _, fileInfo := range filesInfo {
		if fileInfo.IsDir() {
			folder := NewFolder(folder.clientLazy, folder.host, client.Join(folder.path, fileInfo.Name()), folder.user)
			subFolders = append(subFolders, folder)
			// Folder is not object, just skip it
			continue
		}

		object := storage.NewLocalObject(
			fileInfo.Name(),
			fileInfo.ModTime(),
			fileInfo.Size(),
		)
		objects = append(objects, object)
	}

	return objects, subFolders, err
}

func (folder *Folder) DeleteObjects(objectRelativePaths []string) error {
	client, err := folder.clientLazy()
	if err != nil {
		return err
	}

	for _, relativePath := range objectRelativePaths {
		objPath := client.Join(folder.path, relativePath)

		stat, err := client.Stat(objPath)
		if errors.Is(err, os.ErrNotExist) {
			// Don't throw error if the file doesn't exist, to follow the storage.Folder contract
			continue
		}
		if err != nil {
			return NewFolderError(err, "Fail to get object stat '%s': %v", objPath, err)
		}

		// Do not try to remove directory. It may be not empty. TODO: remove if empty
		if stat.IsDir() {
			continue
		}

		err = client.Remove(objPath)
		if errors.Is(err, os.ErrNotExist) {
			// Don't throw error if the file doesn't exist, to follow the storage.Folder contract
			continue
		}
		if err != nil {
			return NewFolderError(err, "Fail delete object '%s': %v", objPath, err)
		}
	}

	return nil
}

func (folder *Folder) Exists(objectRelativePath string) (bool, error) {
	client, err := folder.clientLazy()
	if err != nil {
		return false, err
	}

	objPath := filepath.Join(folder.path, objectRelativePath)
	_, err = client.Stat(objPath)

	if os.IsNotExist(err) {
		return false, nil
	}

	if err != nil {
		return false, NewFolderError(
			err, "Fail check object existence '%s'", objPath,
		)
	}

	return true, nil
}

func (folder *Folder) GetSubFolder(subFolderRelativePath string) storage.Folder {
	return NewFolder(
		folder.clientLazy,
		folder.host,
		path.Join(folder.path, subFolderRelativePath),
		folder.user,
	)
}

func (folder *Folder) ReadObject(objectRelativePath string) (io.ReadCloser, error) {
	client, err := folder.clientLazy()
	if err != nil {
		return nil, err
	}

	objPath := path.Join(folder.path, objectRelativePath)
	file, err := client.OpenFile(objPath)

	if err != nil {
		return nil, storage.NewObjectNotFoundError(objPath)
	}

	return struct {
		io.Reader
		io.Closer
	}{bufio.NewReaderSize(file, defaultBufferSize), file}, nil
}

func (folder *Folder) PutObject(name string, content io.Reader) error {
	client, err := folder.clientLazy()
	if err != nil {
		return err
	}

	absolutePath := filepath.Join(folder.path, name)

	dirPath := filepath.Dir(absolutePath)
	err = client.Mkdir(dirPath)
	if err != nil {
		return NewFolderError(
			err, "Fail to create directory '%s'",
			dirPath,
		)
	}

	file, err := client.CreateFile(absolutePath)
	if err != nil {
		return NewFolderError(
			err, "Fail to create file '%s'",
			absolutePath,
		)
	}

	_, err = io.Copy(file, content)
	if err != nil {
		closerErr := file.Close()
		if closerErr != nil {
			tracelog.InfoLogger.Println("Error during closing failed upload ", closerErr)
		}
		return NewFolderError(
			err, "Fail write content to file '%s'",
			absolutePath,
		)
	}
	err = file.Close()
	if err != nil {
		return NewFolderError(
			err, "Fail write close file '%s'",
			absolutePath,
		)
	}
	return nil
}

func (folder *Folder) PutObjectWithContext(ctx context.Context, name string, content io.Reader) error {
	ctxReader := contextio.NewReader(ctx, content)
	return folder.PutObject(name, ctxReader)
}

func (folder *Folder) CopyObject(srcPath string, dstPath string) error {
	if exists, err := folder.Exists(srcPath); !exists {
		if err == nil {
			return storage.NewObjectNotFoundError(srcPath)
		}
		return err
	}
	file, err := folder.ReadObject(srcPath)
	if err != nil {
		return err
	}
	err = folder.PutObject(dstPath, file)
	if err != nil {
		return err
	}
	return nil
}

func (folder *Folder) MoveObject(srcPath string, dstPath string) error {
	err := folder.CopyObject(srcPath, dstPath)
	if err != nil {
		return err
	}
	err = folder.DeleteObjects([]string{srcPath})
	return err
}

func (folder *Folder) Hash() storage.Hash {
	hash := fnv.New64a()

	addToHash := func(data []byte) {
		_, err := hash.Write(data)
		if err != nil {
			// Writing to the hash function is always successful, so it mustn't be a problem that we panic here
			panic(err)
		}
	}

	addToHash([]byte("sh"))
	addToHash([]byte(folder.host))
	addToHash([]byte(folder.path))
	addToHash([]byte(folder.user))

	return storage.Hash(hash.Sum64())
}
