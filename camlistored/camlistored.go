// Copyright 2010 Brad Fitzpatrick <brad@danga.com>
//
// See LICENSE.

package main

import "crypto/sha1"
import "encoding/base64"
import "flag"
import "fmt"
import "hash"
import "http"
import "io"
import "io/ioutil"
import "os"
import "regexp"

var listen *string = flag.String("listen", "0.0.0.0:3179", "host:port to listen on")
var storageRoot *string = flag.String("root", "/tmp/camliroot", "Root directory to store files")

var sharedSecret string

var kGetPutPattern *regexp.Regexp = regexp.MustCompile(`^/camli/(sha1)-([a-f0-9]+)$`)
var kBasicAuthPattern *regexp.Regexp = regexp.MustCompile(`^Basic ([a-zA-Z0-9\+/=]+)`)

type ObjectRef struct {
	hashName string
	digest   string
};

func ParsePath(path string) *ObjectRef {
	groups := kGetPutPattern.MatchStrings(path)
	if (len(groups) != 3) {
		return nil
	}
	obj := &ObjectRef{groups[1], groups[2]}
	if obj.hashName == "sha1" && len(obj.digest) != 40 {
		return nil
	}
	return obj;
}

func (o *ObjectRef) IsSupported() bool {
	if o.hashName == "sha1" {
		return true
	}
	return false
}

func (o *ObjectRef) Hash() hash.Hash {
	if o.hashName == "sha1" {
		return sha1.New()
	}
	return nil
}

func (o *ObjectRef) FileBaseName() string {
	return fmt.Sprintf("%s-%s.dat", o.hashName, o.digest)
}

func (o *ObjectRef) DirectoryName() string {
	return fmt.Sprintf("%s/%s/%s", *storageRoot, o.digest[0:3], o.digest[3:6])

}

func (o *ObjectRef) FileName() string {
	return fmt.Sprintf("%s/%s-%s.dat", o.DirectoryName(), o.hashName, o.digest)
}

func badRequestError(conn *http.Conn, errorMessage string) {
	conn.WriteHeader(http.StatusBadRequest)
        fmt.Fprintf(conn, "%s\n", errorMessage)
}

func serverError(conn *http.Conn, err os.Error) {
	conn.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(conn, "Server error: %s\n", err)
}

func putAllowed(req *http.Request) bool {
	auth, present := req.Header["Authorization"]
	if !present {
		return false
	}
	matches := kBasicAuthPattern.MatchStrings(auth)
	if len(matches) != 2 {
		return false
	}
	var outBuf []byte = make([]byte, base64.StdEncoding.DecodedLen(len(matches[1])))
	bytes, err := base64.StdEncoding.Decode(outBuf, []uint8(matches[1]))
	fmt.Println("Decoded bytes:", bytes, " error: ", err)
	fmt.Println("Got userPass:", string(outBuf))
	return false
}

func handleCamli(conn *http.Conn, req *http.Request) {
	if (req.Method == "PUT") {
		handlePut(conn, req)
		return
	}

	if (req.Method == "GET") {
		handleGet(conn, req)
		return
	}

	badRequestError(conn, "Unsupported method.")
}

func handleGet(conn *http.Conn, req *http.Request) {
	objRef := ParsePath(req.URL.Path)
	if objRef == nil {
		badRequestError(conn, "Malformed GET URL.")
                return
	}
	fileName := objRef.FileName()
	stat, err := os.Stat(fileName)
	if err == os.ENOENT {
		conn.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(conn, "Object not found.")
		return
	}
	if err != nil {
		serverError(conn, err); return
	}
	file, err := os.Open(fileName, os.O_RDONLY, 0)
	if err != nil {
		serverError(conn, err); return
	}
	conn.SetHeader("Content-Type", "application/octet-stream")
	bytesCopied, err := io.Copy(conn, file)

	// If there's an error at this point, it's too late to tell the client,
	// as they've already been receiving bytes.  But they should be smart enough
	// to verify the digest doesn't match.  But we close the (chunked) response anyway,
	// to further signal errors.
        if err != nil {
		fmt.Fprintf(os.Stderr, "Error sending file: %v, err=%v\n", objRef, err)
		closer, _, err := conn.Hijack()
		if err != nil {	closer.Close() }
                return
        }
	if bytesCopied != stat.Size {
		fmt.Fprintf(os.Stderr, "Error sending file: %v, copied= %d, not %d%v\n", objRef,
			bytesCopied, stat.Size)
		closer, _, err := conn.Hijack()
		if err != nil {	closer.Close() }
                return
	}
}

func handlePut(conn *http.Conn, req *http.Request) {
	objRef := ParsePath(req.URL.Path)
	if objRef == nil {
		badRequestError(conn, "Malformed PUT URL.")
                return
	}

	if !objRef.IsSupported() {
		badRequestError(conn, "unsupported object hash function")
		return
	}

	if !putAllowed(req) {
		conn.SetHeader("WWW-Authenticate", "Basic realm=\"camlistored\"")
		conn.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(conn, "Authentication required.")
		return
	}

	// TODO(bradfitz): authn/authz checks here.

	hashedDirectory := objRef.DirectoryName()
	err := os.MkdirAll(hashedDirectory, 0700)
	if err != nil {
		serverError(conn, err)
		return
	}

	tempFile, err := ioutil.TempFile(hashedDirectory, objRef.FileBaseName() + ".tmp")
	if err != nil {
                serverError(conn, err)
                return
        }

	success := false  // set true later
	defer func() {
		if !success {
			fmt.Println("Removing temp file: ", tempFile.Name())
			os.Remove(tempFile.Name())
		}
	}();

	written, err := io.Copy(tempFile, req.Body)
	if err != nil {
                serverError(conn, err); return
        }
	if _, err = tempFile.Seek(0, 0); err != nil {
		serverError(conn, err); return
	}

	hasher := objRef.Hash()

	io.Copy(hasher, tempFile)
	if fmt.Sprintf("%x", hasher.Sum()) != objRef.digest {
		badRequestError(conn, "digest didn't match as declared.")
		return;
	}
	if err = tempFile.Close(); err != nil {
		serverError(conn, err); return
	}

	fileName := objRef.FileName()
	if err = os.Rename(tempFile.Name(), fileName); err != nil {
		serverError(conn, err); return
	}

	stat, err := os.Lstat(fileName)
	if err != nil {
		serverError(conn, err); return;
	}
	if !stat.IsRegular() || stat.Size != written {
		serverError(conn, os.NewError("Written size didn't match."))
		// Unlink it?  Bogus?  Naah, better to not lose data.
		// We can clean it up later in a GC phase.
		return
	}

	success = true
	fmt.Fprint(conn, "OK")
}

func HandleRoot(conn *http.Conn, req *http.Request) {
	fmt.Fprintf(conn, `
This is camlistored, a Camlistore storage daemon.
`);
}

func main() {
	flag.Parse()

	sharedSecret = os.Getenv("CAMLI_PASSWORD")
	if len(sharedSecret) == 0 {
		fmt.Fprintf(os.Stderr,
			"No CAMLI_PASSWORD environment variable set.\n")
		os.Exit(1)
	}

	{
		fi, err := os.Stat(*storageRoot)
		if err != nil || !fi.IsDirectory() {
			fmt.Fprintf(os.Stderr,
				"Storage root '%s' doesn't exist or is not a directory.\n",
				*storageRoot)
			os.Exit(1)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", HandleRoot)
	mux.HandleFunc("/camli/", handleCamli)

	fmt.Printf("Starting to listen on http://%v/\n", *listen)
	err := http.ListenAndServe(*listen, mux)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"Error in http server: %v\n", err)
		os.Exit(1)
	}
}
