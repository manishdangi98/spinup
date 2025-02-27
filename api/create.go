package api

import (
	"bytes"
	"context"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"github.com/golang-jwt/jwt"
	_ "github.com/mattn/go-sqlite3"
)

var authToken, zoneID, projectDir, architecture string
var api *cloudflare.API
var privKeyPath, pubKeyPath string
var (
	verifyKey *rsa.PublicKey
	signKey   *rsa.PrivateKey
)

func init() {
	var ok bool
	var err error
	if projectDir, ok = os.LookupEnv("SPINUP_PROJECT_DIR"); !ok {
		log.Fatalf("FATAL: getting environment variable SPINUP_PROJECT_DIR")
	}
	if architecture, ok = os.LookupEnv("ARCHITECTURE"); !ok {
		log.Fatalf("FATAL: getting environment variable ARCHITECTURE")
	}
	if authToken, ok = os.LookupEnv("CF_AUTHORIZATION_TOKEN"); !ok {
		log.Fatalf("FATAL: getting environment variable CF_AUTHORIZATION_TOKEN")
	}
	if zoneID, ok = os.LookupEnv("CF_ZONE_ID"); !ok {
		log.Fatalf("FATAL: getting environment variable CF_ZONE_ID")
	}
	api, err = cloudflare.NewWithAPIToken(authToken)
	if err != nil {
		log.Fatalf("FATAL: creating new cloudflare client %v", err)
	}

	signBytes, err := ioutil.ReadFile(projectDir + "/app.rsa")
	fatal(err)

	signKey, err = jwt.ParseRSAPrivateKeyFromPEM(signBytes)
	fatal(err)
	verifyBytes, err := ioutil.ReadFile(projectDir + "/app.rsa.pub")
	fatal(err)

	verifyKey, err = jwt.ParseRSAPublicKeyFromPEM(verifyBytes)
	fatal(err)
	log.Println("INFO: initial validations successful")
}

type service struct {
	Duration time.Duration
	UserID   string
	// one of arm64v8 or arm32v7 or amd64
	Architecture string
	//Port         uint
	Db dbCluster
}

type dbCluster struct {
	Name       string
	ID         string
	Type       string
	Port       int
	MajVersion uint
	MinVersion uint
	Memory     string
	Storage    string
}

type serviceResponse struct {
	HostName    string
	Port        int
	ContainerID string
}

func Hello(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "hello !! Welcome to spinup \n")
}

func CreateService(w http.ResponseWriter, req *http.Request) {
	if (*req).Method != "POST" {
		http.Error(w, "Invalid Method", http.StatusMethodNotAllowed)
		return
	}
	authHeader := req.Header.Get("Authorization")
	userId, err := validateToken(authHeader)
	if err != nil {
		log.Printf("error validating token %v", err)
		http.Error(w, "error validating token", 500)
	}
	var s service
	byteArray, err := ioutil.ReadAll(req.Body)
	if err != nil {
		log.Fatalf("fatal: reading from readall body %v", err)
	}
	err = json.Unmarshal(byteArray, &s)
	if err != nil {
		log.Fatalf("fatal: reading from readall body %v", err)
	}
	if s.UserID != userId {
		log.Printf("user %s trying to access /createservice using jwt userId %s", s.UserID, userId)
		http.Error(w, "userid doesn't match", http.StatusInternalServerError)
		return
	}
	if s.Db.Type != "postgres" {
		fmt.Fprintf(w, "currently we don't support %s", s.Db.Type)
		http.Error(w, "db type is currently not supported", 500)
		return
	}
	s.Db.Port, err = portcheck()
	s.Architecture = architecture
	servicePath := projectDir + "/" + s.UserID + "/" + s.Db.Name
	if err = prepareService(s, servicePath); err != nil {
		log.Printf("ERROR: preparing service for %s %v", s.UserID, err)
		http.Error(w, "Error preparing service", 500)
		return
	}
	if err = startService(s, servicePath); err != nil {
		log.Printf("ERROR: starting service for %s %v", s.UserID, err)
		http.Error(w, "Error starting service", 500)
		return
	}
	log.Printf("INFO: created service for user %s", s.UserID)
	/* err = connectService(s)
	if err != nil {
		log.Printf("ERROR: connecting service for %s %v", s.UserID, err)
		http.Error(w, "Error connecting service", 500)
		return
	}
	err = internal.UpdateTunnelClientYml(s.Db.Name, s.Db.Port)
	if err != nil {
		log.Printf("ERROR: updating tunnel client for %s %v", s.UserID, err)
		http.Error(w, "Error updating tunnel client", 500)
		return
	} */
	containerID, err := lastContainerID()
	if err != nil {
		log.Printf("ERROR: getting container id %v", err)
		http.Error(w, "Error getting container id", 500)
		return
	}
	s.Db.ID = containerID
	var serRes serviceResponse
	//serRes.HostName = s.UserID + "-" + s.Db.Name + ".spinup.host"
	serRes.HostName = "localhost"
	serRes.Port = s.Db.Port
	serRes.ContainerID = containerID
	jsonBody, err := json.Marshal(serRes)
	if err != nil {
		log.Printf("ERROR: marshalling service response struct serviceResponse %v", err)
		http.Error(w, "Internal server error ", 500)
		return
	}
	updateSqliteDB(projectDir+"/"+s.UserID, s.UserID, s)
	w.Write(jsonBody)
}

func prepareService(s service, path string) error {
	err := os.MkdirAll(path, 0755)
	if err != nil {
		return fmt.Errorf("ERROR: creating project directory at %s", path)
	}
	if err := createDockerComposeFile(path, s); err != nil {
		return fmt.Errorf("ERROR: creating service docker-compose file %v", err)
	}
	return nil
}

func startService(s service, path string) error {
	err := ValidateSystemRequirements()
	if err != nil {
		return err
	}
	err = ValidateDockerCompose(path)
	if err != nil {
		return err
	}
	cmd := exec.Command("docker-compose", "-f", path+"/docker-compose.yml", "up", "-d")
	// https://stackoverflow.com/questions/18159704/how-to-debug-exit-status-1-error-when-running-exec-command-in-golang/18159705
	// To print the actual error instead of just printing the exit status
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		fmt.Println(fmt.Sprint(err) + ": " + stderr.String())
		return err
	}
	return nil
}

func ValidateDockerCompose(path string) error {
	cmd := exec.Command("docker-compose", "-f", path+"/docker-compose.yml", "config")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("validating docker-compose file %v", err)
	}
	return nil
}

func ValidateSystemRequirements() error {
	cmd := exec.Command("which", "docker-compose")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker-compose doesn't exist %v", err)
	}
	cmd = exec.Command("which", "docker")
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func connectService(s service) error {
	_, err := api.CreateDNSRecord(context.Background(), zoneID, cloudflare.DNSRecord{
		Type:    "A",
		Name:    s.UserID + "-" + s.Db.Name,
		Content: "34.203.202.32",
	})
	if err != nil {
		return err
	}
	log.Printf("INFO: DNS record created for %s ", s.UserID+"-"+s.Db.Name)
	return nil
}

func portcheck() (int, error) {
	endingPort := 5440
	for startingPort := 5432; startingPort < endingPort; startingPort++ {
		target := fmt.Sprintf("%s:%d", "localhost", startingPort)
		_, err := net.DialTimeout("tcp", target, 3*time.Second)
		if err != nil && !strings.Contains(err.Error(), "connect: connection refused") {
			log.Printf("INFO: error on port scanning %d %v", startingPort, err)
			return 0, err
		}
		if err != nil && strings.Contains(err.Error(), "connect: connection refused") {
			log.Printf("INFO: port %d is unused", startingPort)
			return startingPort, nil
		}
	}
	log.Printf("WARN: all allocated ports are occupied")
	return 0, fmt.Errorf("error all allocated ports are occupied")
}

func lastContainerID() (string, error) {
	cmd := exec.Command("/bin/bash", "-c", "docker ps --last 1 -q")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func validateToken(authHeader string) (string, error) {
	splitToken := strings.Split(authHeader, "Bearer ")
	if len(splitToken) < 2 {
		return "", fmt.Errorf("cannot validate empty token")
	}
	reqToken := splitToken[1]
	userID, err := JWTToString(reqToken)
	if err != nil {
		return "", err
	}
	return userID, nil
}

func updateSqliteDB(path string, dbName string, data service) {
	db, err := sql.Open("sqlite3", path+"/"+dbName+".db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	sqlStmt := `
	create table if not exists clusterInfo (id integer not null primary key autoincrement, clusterId text, Name text, Port integer);
	`
	_, err = db.Exec(sqlStmt)
	if err != nil {
		log.Printf("%q: %s\n", err, sqlStmt)
	}
	tx, err := db.Begin()
	if err != nil {
		log.Fatal(err)
	}
	stmt, err := tx.Prepare("insert into clusterInfo(clusterId, name, port) values(?, ?, ?)")
	if err != nil {
		log.Fatal(err)
	}
	defer stmt.Close()
	_, err = stmt.Exec(data.Db.ID, data.Db.Name, data.Db.Port)
	if err != nil {
		log.Fatal(err)
	}
	tx.Commit()
}
