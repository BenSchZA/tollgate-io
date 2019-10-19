package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/iotaledger/iota.go/trinary"

	// "github.com/didip/tollbooth"
	// "github.com/throttled/throttled"
	"github.com/boltdb/bolt"
	"golang.org/x/time/rate"
)

//https://www.alexedwards.net/blog/how-to-rate-limit-http-requests

// Change the the map to hold values of the type Session.
var sessions = make(map[string]*Session)
var mtx sync.Mutex

// Run a background goroutine to remove old entries from the sessions map.
func init() {
	go cleanupSessions()
}

func addSession(ip string, consumer string, producer string) *Session {
	limiter := rate.NewLimiter(2, 5)
	mtx.Lock()
	// Include the current time when creating a new Session.
	value := Session{
		id:             ip,
		limiter:        limiter,
		lastSeen:       time.Now(),
		consumer:       consumer,
		producer:       producer,
		initial_value:  GetBalance(consumer),
		paid_value:     0,
		expected_value: 0,
	}
	sessions[ip] = &value
	mtx.Unlock()
	log.Println("New Session:", ip)
	return &value
}

func getSession(ip string, consumer string, producer string) *Session {
	mtx.Lock()
	v, exists := sessions[ip]
	if !exists {
		mtx.Unlock()
		return addSession(ip, consumer, producer)
	} else {
		log.Println("Session:", v)
	}

	// Update the last seen time for the Session.
	v.lastSeen = time.Now()
	mtx.Unlock()
	return v
}

// Every minute check the map for sessions that haven't been seen for
// more than 10 minutes and delete the entries.
func cleanupSessions() {
	for {
		time.Sleep(time.Minute)
		mtx.Lock()
		for ip, v := range sessions {
			if time.Now().Sub(v.lastSeen) > 10*time.Minute {
				delete(sessions, ip)
			}
		}
		mtx.Unlock()
	}
}

var txPrice uint64 = 1
var txBuffer uint64 = 10

func validateTransaction(session *Session) bool {
	session.expected_value = session.expected_value + txPrice
	session.paid_value = GetBalance(session.consumer) - session.initial_value //TODO: set as tx between
	sessions[session.id] = session
	if session.expected_value-session.paid_value > txBuffer {
		return false
	} else {
		return true
	}
}

var seeds = []Endpoint{
	Endpoint{
		Id:      "a",
		Url:     "https://alpha-api-nightly.mol.ai",
		Address: "FMYHLHBSJJMJZNPVUOKDCUSFOPQAGPBSPOPMFVBGXUUDFPEWPXREZFQKGKSNHZWDMODRDYWIXQT9CLVBXGPANCSYBW",
	},
	Endpoint{
		Id:      "b",
		Url:     "https://google.com",
		Address: "FMYHLHBSJJMJZNPVUOKDCUSFOPQAGPBSPOPMFVBGXUUDFPEWPXREZFQKGKSNHZWDMODRDYWIXQT9CLVBXGPANCSYBW",
	},
}

func seed_db(db *bolt.DB) {
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("APIS"))
		var err error
		for _, api := range seeds {
			encoded, err_json := json.Marshal(api)
			must(err_json)
			log.Println("Seeding:", api.Id, api)

			err = b.Put([]byte(api.Id), encoded)
			must(err)
		}
		return err
	})
	must(err)
}

func create_buckets(db *bolt.DB) {
	db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("Sessions"))
		if err != nil {
			return fmt.Errorf("Create bucket: %s", err)
		}
		return nil
	})
	db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("APIS"))
		if err != nil {
			return fmt.Errorf("Create bucket: %s", err)
		}
		return nil
	})
}

var db *bolt.DB
var router *mux.Router

func main() {
	var err error
	db, err = bolt.Open("store.db", 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Initialize datastore
	create_buckets(db)
	seed_db(db)

	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("APIS"))
		v := b.Get([]byte("a"))
		var data *Endpoint
		json.Unmarshal(v, &data)
		fmt.Printf("Value for key 'a': %s\n", data)
		return nil
	})

	router = mux.NewRouter()
	router.HandleFunc("/balance/{address}", get_balance_handler)
	router.HandleFunc("/endpoint/{id}/{path:.*}", proxy_handler)

	err = http.ListenAndServe(":8080", router)
	if err != nil {
		panic(err)
	}
}

func get_balance_handler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)

	address := trinary.Trytes(vars["address"])
	balance := GetBalance(address)
	log.Println("Balance:", balance)
}

func proxy_handler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	// Use IP for now
	// key := vars["apiKey"] //TODO: we need to generate an API key with consumer seed for Session
	id := vars["id"]
	path := vars["path"]

	var p *httputil.ReverseProxy
	var apiData *Endpoint

	err_endpoint := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("APIS"))
		v := b.Get([]byte(id))

		if err := json.Unmarshal(v, &apiData); err != nil {
			return err
		}

		log.Println("Endpoint:", apiData)

		remote, err := url.Parse(string(apiData.Url))
		must(err)
		p = httputil.NewSingleHostReverseProxy(remote)

		producer_balance := GetBalance(apiData.Address)
		log.Println("Producer balance:", producer_balance)

		return nil
	})
	if err_endpoint != nil {
		http.Error(w, http.StatusText(404), http.StatusNotFound)
		return
	}

	session := getSession(
		req.RemoteAddr,
		"JXBIEWEBYCZOKBHIGDXT9VNLUTGCZGXJLCSAUTCRGEEHFETHRIVMTBNKGPQUXNVSCLIWEKHWFBASGYFLWZOGJE9YPX",
		apiData.Address,
	)
	validTX := validateTransaction(session)
	if validTX {
		log.Println("Valid TX:", session.id, session.expected_value)
	}
	limiter := session.limiter
	if limiter.Allow() == false || !validTX {
		http.Error(w, http.StatusText(429), http.StatusTooManyRequests)
		return
	}

	log.Println("Getting path", path, "from Endpoint with ID", id)

	req.Host = ""
	req.URL.Path = path

	p.ServeHTTP(w, req)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
