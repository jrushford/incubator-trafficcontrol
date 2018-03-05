package main

/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/apache/incubator-trafficcontrol/lib/go-log"
	"github.com/apache/incubator-trafficcontrol/lib/go-tc"
	"github.com/apache/incubator-trafficcontrol/traffic_ops/traffic_ops_golang/api"
	"github.com/apache/incubator-trafficcontrol/traffic_ops/traffic_ops_golang/auth"
	"github.com/apache/incubator-trafficcontrol/traffic_ops/traffic_ops_golang/tenant"
	"github.com/basho/riak-go-client"
	"github.com/jmoiron/sqlx"
)

// Delivery Services: SSL Keys.

func publicKey(priv interface{}) interface{} {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	default:
		return nil
	}
}

// generates an unencrypted private key and a signing request, CSR
func generateSSLCertificate(sslKeys *tc.DeliveryServiceSSLKeys) error {
	// generate the private key.
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)

	country := []string{sslKeys.Country}
	province := []string{sslKeys.State}
	locality := []string{sslKeys.City}
	organization := []string{sslKeys.Organization}
	organizationUnit := []string{sslKeys.BusinessUnit}

	// generate a self signed certificate.
	var validFor = 365 * 24 * time.Hour
	var notBefore = time.Now()
	notAfter := notBefore.Add(validFor)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		log.Errorf("failed to generate serial number: %s", err)
		return err
	}

	crt_template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:         sslKeys.Hostname,
			Country:            country,
			Province:           province,
			Locality:           locality,
			Organization:       organization,
			OrganizationalUnit: organizationUnit,
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	crt_template.DNSNames = append(crt_template.DNSNames, sslKeys.Hostname)
	crtBlock := pem.Block{
		Type: "CERTIFICATE",
	}
	crtBytes, err := x509.CreateCertificate(rand.Reader, &crt_template, &crt_template, publicKey(privateKey), privateKey)
	if err != nil {
		log.Errorf("failed to create certificate: %s", err)
		return err
	} else {
		crtBlock.Bytes = crtBytes
	}
	// pem encode the CRT
	crtPem := pem.EncodeToMemory(&crtBlock)

	// data needed for a signing request, CSR
	subj := pkix.Name{
		CommonName:         sslKeys.Hostname,
		Country:            country,
		Province:           province,
		Locality:           locality,
		Organization:       organization,
		OrganizationalUnit: organizationUnit}

	// create the CSR subject
	rawSubj := subj.ToRDNSequence()
	asn1Subj, _ := asn1.Marshal(rawSubj)
	template := x509.CertificateRequest{
		RawSubject:         asn1Subj,
		SignatureAlgorithm: x509.SHA256WithRSA,
	}

	// create the CSR
	csrBlock := pem.Block{
		Type: "CERTIFICATE REQUEST",
	}
	csrBytes, err := x509.CreateCertificateRequest(rand.Reader, &template, privateKey)
	if err != nil {
		log.Errorf("failed to create certificate request: %s", err)
		return err
	} else {
		csrBlock.Bytes = csrBytes
	}
	// pem encode the CSR
	csrPem := pem.EncodeToMemory(&csrBlock)

	// pem encode the private key
	privKeyBlock := pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}
	privKeyPem := pem.EncodeToMemory(&privKeyBlock)

	// base64 encode the private key and CSR.
	cert := tc.DeliveryServiceSSLKeysCertificate{
		Key: base64.StdEncoding.EncodeToString(privKeyPem),
		Crt: base64.StdEncoding.EncodeToString(crtPem),
		CSR: base64.StdEncoding.EncodeToString(csrPem),
	}
	// finally assign the result.
	sslKeys.Certificate = cert

	return nil
}

// returns the cdn_id found by domainname.
func getCDNIDByDomainname(domainName string, db *sqlx.DB) (sql.NullInt64, error) {
	cdnQuery := `SELECT id from cdn WHERE domain_name = $1`
	var cdnID sql.NullInt64

	noCdnID := sql.NullInt64{
		Int64: 0,
		Valid: false,
	}

	rows, err := db.Query(cdnQuery, domainName)
	if err != nil {
		return noCdnID, err
	}
	defer rows.Close()

	for rows.Next() {
		if err := rows.Scan(&cdnID); err != nil {
			return noCdnID, err
		}
	}

	return cdnID, nil
}

// returns a delivery service xmlId for a cdn by host regex.
func getXMLID(cdnID sql.NullInt64, hostRegex string, db *sqlx.DB) (sql.NullString, error) {
	dsQuery := `
			SELECT ds.xml_id from deliveryservice ds
			INNER JOIN deliveryservice_regex dr 
			on ds.id = dr.deliveryservice AND ds.cdn_id = $1
			INNER JOIN regex r on r.id = dr.regex
			WHERE r.pattern = $2
		`
	var xmlID sql.NullString

	rows, err := db.Query(dsQuery, cdnID.Int64, hostRegex)
	if err != nil {
		xmlID.Valid = false
		return xmlID, err
	}
	defer rows.Close()

	for rows.Next() {
		if err := rows.Scan(&xmlID); err != nil {
			xmlID.Valid = false
			return xmlID, err
		}
	}

	return xmlID, nil
}

func getDeliveryServiceSSLKeysByXMLID(xmlID string, version string, db *sqlx.DB, cfg Config) ([]byte, error) {
	var respBytes []byte
	// create and start a cluster
	cluster, err := getRiakCluster(db, cfg)
	if err != nil {
		return nil, err
	}
	if err = cluster.Start(); err != nil {
		return nil, err
	}
	defer func() {
		if err := cluster.Stop(); err != nil {
			log.Errorf("%v\n", err)
		}
	}()

	if version == "" {
		xmlID = xmlID + "-latest"
	} else {
		xmlID = xmlID + "-" + version
	}

	// get the deliveryservice ssl keys by xmlID and version
	ro, err := fetchObjectValues(xmlID, SSLKeysBucket, cluster)
	if err != nil {
		return nil, err
	}

	// no keys we're found
	if ro == nil {
		alert := tc.CreateAlerts(tc.InfoLevel, "no object found for the specified key")
		respBytes, err = json.Marshal(alert)
		if err != nil {
			log.Errorf("failed to marshal an alert response: %s\n", err)
			return nil, err
		}
	} else { // keys were found
		var key tc.DeliveryServiceSSLKeys

		// unmarshal into a response tc.DeliveryServiceSSLKeysResponse object.
		if err := json.Unmarshal(ro[0].Value, &key); err != nil {
			log.Errorf("failed at unmarshaling sslkey response: %s\n", err)
			return nil, err
		}
		resp := tc.DeliveryServiceSSLKeysResponse{
			Response: key,
		}
		respBytes, err = json.Marshal(resp)
		if err != nil {
			log.Errorf("failed to marshal a sslkeys response: %s\n", err)
			return nil, err
		}
	}

	return respBytes, nil
}

// verify the server certificate chain and return the
// certificate and its chain in the proper order. Returns a  verified,
// ordered, and base64 encoded certificate and CA chain.
func verifyAndEncodeCertificate(certificate string, rootCA string) (string, error) {
	var pemEncodedChain string
	var b64crt string

	// strip newlines from encoded crt and decode it from base64.
	crtArr := strings.Split(certificate, "\\n")
	for i := 0; i < len(crtArr); i++ {
		b64crt += crtArr[i]
	}
	pemCerts := make([]byte, base64.StdEncoding.EncodedLen(len(b64crt)))
	_, err := base64.StdEncoding.Decode(pemCerts, []byte(b64crt))
	if err != nil {
		return "", fmt.Errorf("could not base64 decode the certificate %v", err)
	}

	// decode, verify, and order certs for storgae
	var bundle string
	certs := strings.SplitAfter(string(pemCerts), "-----END CERTIFICATE-----")
	if len(certs) > 1 {
		// decode and verify the server certificate
		block, _ := pem.Decode([]byte(certs[0]))
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", fmt.Errorf("could not parse the server certificate %v", err)
		}
		if !(cert.KeyUsage&x509.KeyUsageKeyEncipherment > 0) {
			return "", fmt.Errorf("no key encipherment usage for the server certificate")
		}
		for i := 0; i < len(certs)-1; i++ {
			bundle += certs[i]
		}

		var opts x509.VerifyOptions

		rootPool := x509.NewCertPool()
		if rootCA != "" {
			if !rootPool.AppendCertsFromPEM([]byte(rootCA)) {
				return "", fmt.Errorf("root  CA certificate is empty, %v", err)
			}
		}

		intermediatePool := x509.NewCertPool()
		if !intermediatePool.AppendCertsFromPEM([]byte(bundle)) {
			return "", fmt.Errorf("certificate CA bundle is empty, %v", err)
		}

		if rootCA != "" {
			// verify the certificate chain.
			opts = x509.VerifyOptions{
				Intermediates: intermediatePool,
				Roots:         rootPool,
			}
		} else {
			opts = x509.VerifyOptions{
				Intermediates: intermediatePool,
			}
		}

		chain, err := cert.Verify(opts)
		if err != nil {
			return "", fmt.Errorf("could verify the certificate chain %v", err)
		}
		if len(chain) > 0 {
			for _, link := range chain[0] {
				// Only print non-self signed elements of the chain
				if link.AuthorityKeyId != nil && !bytes.Equal(link.AuthorityKeyId, link.SubjectKeyId) {
					block := &pem.Block{Type: "CERTIFICATE", Bytes: link.Raw}
					pemEncodedChain += string(pem.EncodeToMemory(block))
				}
			}
		} else {
			return "", fmt.Errorf("Can't find valid chain for cert in file in request")
		}
	} else {
		return "", fmt.Errorf("ERROR: no certificate chain to verify")
	}

	base64EncodedStr := base64.StdEncoding.EncodeToString([]byte(pemEncodedChain))

	return base64EncodedStr, nil
}

func addDeliveryServiceSSLKeysHandler(db *sqlx.DB, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleErr := tc.GetHandleErrorsFunc(w, r)
		var keysObj tc.DeliveryServiceSSLKeys

		if cfg.RiakEnabled == false {
			handleErr(http.StatusServiceUnavailable, fmt.Errorf("The RIAK service is unavailable"))
			return
		}

		defer r.Body.Close()

		ctx := r.Context()
		user, err := auth.GetCurrentUser(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		// unmarshal the request
		if err := json.Unmarshal(data, &keysObj); err != nil {
			log.Errorf("ERROR: could not unmarshal the request, %v\n", err)
			handleErr(http.StatusBadRequest, err)
			return
		}

		// check user tenancy access to this resource.
		hasAccess, err, apiStatus := tenant.HasTenant(*user, keysObj.DeliveryService, db)
		if !hasAccess {
			switch apiStatus {
			case tc.SystemError:
				handleErr(http.StatusInternalServerError, err)
				return
			case tc.DataMissingError:
				handleErr(http.StatusNotFound, err)
				return
			case tc.ForbiddenError:
				handleErr(http.StatusForbidden, err)
				return
			}
		}

		var certChain string
		if certChain, err = verifyAndEncodeCertificate(keysObj.Certificate.Crt, ""); err != nil {
			log.Errorf("ERROR: could not unmarshal the request, %v\n", err)
			handleErr(http.StatusBadRequest, err)
			return
		}
		keysObj.Certificate.Crt = certChain

		// marshal the keysObj
		keysJSON, err := json.Marshal(&keysObj)
		if err != nil {
			log.Errorf("ERROR: could not marshal the keys object, %v\n", err)
			handleErr(http.StatusBadRequest, err)
			return
		}

		// create and start a cluster
		cluster, err := getRiakCluster(db, cfg)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		if err = cluster.Start(); err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		defer func() {
			if err := cluster.Stop(); err != nil {
				log.Errorf("%v\n", err)
			}
		}()

		// create a storage object and store the data
		obj := &riak.Object{
			ContentType:     "text/json",
			Charset:         "utf-8",
			ContentEncoding: "utf-8",
			Key:             keysObj.DeliveryService,
			Value:           []byte(keysJSON),
		}

		err = saveObject(obj, SSLKeysBucket, cluster)
		if err != nil {
			log.Errorf("%v\n", err)
			handleErr(http.StatusInternalServerError, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "%s", keysJSON)
	}
}

// handler to delete SSL Keys for a delivery service.
func deleteDeliveryServiceSSLKeysHandler(db *sqlx.DB, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleErr := tc.GetHandleErrorsFunc(w, r)

		log.Errorf("deleteDeliveryServiceSSLKeysHandler()")

		if cfg.RiakEnabled == false {
			handleErr(http.StatusServiceUnavailable, fmt.Errorf("The RIAK service is unavailable"))
			return
		}

		version := r.URL.Query().Get("version")

		ctx := r.Context()
		user, err := auth.GetCurrentUser(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		pathParams, err := api.GetPathParams(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		xmlID := pathParams["xmlID"]

		// check user tenancy access to this resource.
		hasAccess, err, apiStatus := tenant.HasTenant(*user, xmlID, db)
		if !hasAccess {
			switch apiStatus {
			case tc.SystemError:
				handleErr(http.StatusInternalServerError, err)
				return
			case tc.DataMissingError:
				handleErr(http.StatusNotFound, err)
				return
			case tc.ForbiddenError:
				handleErr(http.StatusForbidden, err)
				return
			}
		}

		if version == "" {
			xmlID = xmlID + "-latest"
		} else {
			xmlID = xmlID + "-" + version
		}

		// create and start a cluster
		cluster, err := getRiakCluster(db, cfg)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		if err = cluster.Start(); err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		defer func() {
			if err := cluster.Stop(); err != nil {
				log.Errorf("%v\n", err)
			}
		}()

		ro, err := fetchObjectValues(xmlID, SSLKeysBucket, cluster)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		var alert tc.Alerts

		if ro == nil || ro[0].Value == nil {
			alert = tc.CreateAlerts(tc.InfoLevel, "not deleted, no object found to delete")
		} else if err := deleteObject(xmlID, SSLKeysBucket, cluster); err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		} else { // object successfully deleted
			alert = tc.CreateAlerts(tc.SuccessLevel, "object deleted")
		}

		// send response
		respBytes, err := json.Marshal(alert)
		if err != nil {
			log.Errorf("failed to marshal an alert response: %s\n", err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, http.StatusText(http.StatusInternalServerError))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "%s", respBytes)
	}
}

func generateDeliveryServiceSSLKeysHandler(db *sqlx.DB, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleErr := tc.GetHandleErrorsFunc(w, r)
		var keysObj tc.DeliveryServiceSSLKeys

		if cfg.RiakEnabled == false {
			handleErr(http.StatusServiceUnavailable, fmt.Errorf("The RIAK service is unavailable"))
			return
		}

		defer r.Body.Close()

		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		// unmarshal the request
		if err := json.Unmarshal(data, &keysObj); err != nil {
			log.Errorf("ERROR: could not unmarshal the request, %v\n", err)
			handleErr(http.StatusBadRequest, err)
			return
		}
		xmlID := keysObj.DeliveryService

		ctx := r.Context()
		user, err := auth.GetCurrentUser(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		// check user tenancy access to this resource.
		hasAccess, err, apiStatus := tenant.HasTenant(*user, xmlID, db)
		if !hasAccess {
			switch apiStatus {
			case tc.SystemError:
				handleErr(http.StatusInternalServerError, err)
				return
			case tc.DataMissingError:
				handleErr(http.StatusNotFound, err)
				return
			case tc.ForbiddenError:
				handleErr(http.StatusForbidden, err)
				return
			}
		}

		err = generateSSLCertificate(&keysObj)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
	}
}

// fetch the ssl keys for a deliveryservice specified by the fully qualified hostname
func getDeliveryServiceSSLKeysByHostNameHandler(db *sqlx.DB, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleErr := tc.GetHandleErrorsFunc(w, r)
		var respBytes []byte
		var domainName string
		var hostName string
		var hostRegex string

		log.Errorf("getDeliveryServiceSSLKeysByHostNameHandler()")

		if cfg.RiakEnabled == false {
			handleErr(http.StatusServiceUnavailable, fmt.Errorf("The RIAK service is unavailable"))
			return
		}

		version := r.URL.Query().Get("version")

		ctx := r.Context()
		user, err := auth.GetCurrentUser(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		pathParams, err := api.GetPathParams(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		hostName = pathParams["hostName"]

		strArr := strings.Split(hostName, ".")
		ln := len(strArr)

		if ln > 1 {
			for i := 2; i < ln-1; i++ {
				domainName += strArr[i] + "."
			}
			domainName += strArr[ln-1]
			hostRegex = ".*\\." + strArr[1] + "\\..*"
		}

		// lookup the cdnID
		cdnID, err := getCDNIDByDomainname(domainName, db)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		// verify that a valid cdnID was returned.
		if !cdnID.Valid {
			alert := tc.CreateAlerts(tc.InfoLevel, fmt.Sprintf(" - a cdn does not exist for the domain: %s parsed from hostname: %s",
				domainName, hostName))
			respBytes, err = json.Marshal(alert)
			if err != nil {
				log.Errorf("failed to marshal an alert response: %s\n", err)
				return
			}
		} else {
			// now lookup the deliveryservice xmlID
			xmlIDStr, err := getXMLID(cdnID, hostRegex, db)
			if err != nil {
				handleErr(http.StatusInternalServerError, err)
				return
			}

			// verify that the xmlIDStr returned is valid, ie not nil
			if !xmlIDStr.Valid {
				alert := tc.CreateAlerts(tc.InfoLevel, fmt.Sprintf("  - a delivery service does not exist for a host with hostname of %s",
					hostName))
				respBytes, err = json.Marshal(alert)
				if err != nil {
					log.Errorf("failed to marshal an alert response: %s\n", err)
					handleErr(http.StatusInternalServerError, err)
					return
				}
			} else {
				xmlID := xmlIDStr.String
				// check user tenancy access to this resource.
				hasAccess, err, apiStatus := tenant.HasTenant(*user, xmlID, db)
				if !hasAccess {
					switch apiStatus {
					case tc.SystemError:
						handleErr(http.StatusInternalServerError, err)
						return
					case tc.DataMissingError:
						handleErr(http.StatusNotFound, err)
						return
					case tc.ForbiddenError:
						handleErr(http.StatusForbidden, err)
						return
					}
				}
				respBytes, err = getDeliveryServiceSSLKeysByXMLID(xmlID, version, db, cfg)
				if err != nil {
					handleErr(http.StatusInternalServerError, err)
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "%s", respBytes)
	}
}

// fetch the deliveryservice ssl keys by the specified xmlID.
func getDeliveryServiceSSLKeysByXMLIDHandler(db *sqlx.DB, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleErr := tc.GetHandleErrorsFunc(w, r)
		var respBytes []byte

		if cfg.RiakEnabled == false {
			handleErr(http.StatusServiceUnavailable, fmt.Errorf("The RIAK service is unavailable"))
			return
		}

		version := r.URL.Query().Get("version")

		ctx := r.Context()
		user, err := auth.GetCurrentUser(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		pathParams, err := api.GetPathParams(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		xmlID := pathParams["xmlID"]

		// check user tenancy access to this resource.
		hasAccess, err, apiStatus := tenant.HasTenant(*user, xmlID, db)
		if !hasAccess {
			switch apiStatus {
			case tc.SystemError:
				handleErr(http.StatusInternalServerError, err)
				return
			case tc.DataMissingError:
				handleErr(http.StatusNotFound, err)
				return
			case tc.ForbiddenError:
				handleErr(http.StatusForbidden, err)
				return
			}
		}

		respBytes, err = getDeliveryServiceSSLKeysByXMLID(xmlID, version, db, cfg)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "%s", respBytes)
	}
}

// Delivery Services: URI Sign Keys.

// Http POST or PUT handler used to store urisigning keys to a delivery service.
func saveDeliveryServiceURIKeysHandler(db *sqlx.DB, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleErr := tc.GetHandleErrorsFunc(w, r)

		defer r.Body.Close()

		if cfg.RiakEnabled == false {
			handleErr(http.StatusServiceUnavailable, fmt.Errorf("The RIAK service is unavailable"))
			return
		}

		ctx := r.Context()
		pathParams, err := api.GetPathParams(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		xmlID := pathParams["xmlID"]
		data, err := ioutil.ReadAll(r.Body)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		// validate that the received data is a valid jwk keyset
		var keySet map[string]URISignerKeyset
		if err := json.Unmarshal(data, &keySet); err != nil {
			log.Errorf("%v\n", err)
			handleErr(http.StatusBadRequest, err)
			return
		}
		if err := validateURIKeyset(keySet); err != nil {
			log.Errorf("%v\n", err)
			handleErr(http.StatusBadRequest, err)
			return
		}

		// create and start a cluster
		cluster, err := getRiakCluster(db, cfg)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		if err = cluster.Start(); err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		defer func() {
			if err := cluster.Stop(); err != nil {
				log.Errorf("%v\n", err)
			}
		}()

		// create a storage object and store the data
		obj := &riak.Object{
			ContentType:     "text/json",
			Charset:         "utf-8",
			ContentEncoding: "utf-8",
			Key:             xmlID,
			Value:           []byte(data),
		}

		err = saveObject(obj, CDNURIKeysBucket, cluster)
		if err != nil {
			log.Errorf("%v\n", err)
			handleErr(http.StatusInternalServerError, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "%s", data)
	}
}

// endpoint handler for fetching uri signing keys from riak
func getURIsignkeysHandler(db *sqlx.DB, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleErr := tc.GetHandleErrorsFunc(w, r)

		if cfg.RiakEnabled == false {
			handleErr(http.StatusServiceUnavailable, fmt.Errorf("The RIAK service is unavailable"))
			return
		}

		ctx := r.Context()
		pathParams, err := api.GetPathParams(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		xmlID := pathParams["xmlID"]

		// create and start a cluster
		cluster, err := getRiakCluster(db, cfg)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		if err = cluster.Start(); err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		defer func() {
			if err := cluster.Stop(); err != nil {
				log.Errorf("%v\n", err)
			}
		}()

		ro, err := fetchObjectValues(xmlID, CDNURIKeysBucket, cluster)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		var respBytes []byte

		if ro == nil {
			var empty URISignerKeyset
			respBytes, err = json.Marshal(empty)
			if err != nil {
				log.Errorf("failed to marshal an empty response: %s\n", err)
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(w, http.StatusText(http.StatusInternalServerError))
				return
			}
		} else {
			respBytes = ro[0].Value
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "%s", respBytes)
	}
}

// Http DELETE handler used to remove urisigning keys assigned to a delivery service.
func removeDeliveryServiceURIKeysHandler(db *sqlx.DB, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handleErr := tc.GetHandleErrorsFunc(w, r)

		if cfg.RiakEnabled == false {
			handleErr(http.StatusServiceUnavailable, fmt.Errorf("The RIAK service is unavailable"))
			return
		}

		ctx := r.Context()
		pathParams, err := api.GetPathParams(ctx)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		xmlID := pathParams["xmlID"]

		// create and start a cluster
		cluster, err := getRiakCluster(db, cfg)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		if err = cluster.Start(); err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}
		defer func() {
			if err := cluster.Stop(); err != nil {
				log.Errorf("%v\n", err)
			}
		}()

		// fetch the object and delete it if it exists.
		ro, err := fetchObjectValues(xmlID, CDNURIKeysBucket, cluster)
		if err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		}

		var alert tc.Alerts

		if ro == nil || ro[0].Value == nil {
			alert = tc.CreateAlerts(tc.InfoLevel, "not deleted, no object found to delete")
		} else if err := deleteObject(xmlID, CDNURIKeysBucket, cluster); err != nil {
			handleErr(http.StatusInternalServerError, err)
			return
		} else { // object successfully deleted
			alert = tc.CreateAlerts(tc.SuccessLevel, "object deleted")
		}

		// send response
		respBytes, err := json.Marshal(alert)
		if err != nil {
			log.Errorf("failed to marshal an alert response: %s\n", err)
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, http.StatusText(http.StatusInternalServerError))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "%s", respBytes)
	}
}
