package main

// Interactive setup, modelled on Icinga's node wizard. `plv wizard` (handy as
// `docker run -it --rm -v ./data:/data ghcr.io/ngn-au/plv wizard`) asks the role
// and provisions the data dir: a receiver inits the CA + server cert; a forwarder
// verifies the receiver against its CA (ca.pem, copied out-of-band), then enrols
// with a ticket.

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func runWizard(dataDir string) {
	in := bufio.NewReader(os.Stdin)
	fmt.Printf("PLV %s — node wizard (data dir: %s)\n", version, dataDir)
	switch strings.ToLower(prompt(in, "Set up this node as [r]eceiver or [f]orwarder?", "")) {
	case "r", "receiver":
		wizardReceiver(in, dataDir)
	case "f", "forwarder":
		wizardForwarder(in, dataDir)
	default:
		fmt.Fprintln(os.Stderr, "unknown role; expected 'r' or 'f'")
		os.Exit(1)
	}
}

func wizardReceiver(in *bufio.Reader, dataDir string) {
	if fileExists(dataPath(dataDir, caCertFile)) {
		fmt.Printf("A CA already exists in %s — keeping it.\n", dataDir)
	} else if err := pkiInit(dataDir); err != nil {
		fatalErr(err)
	} else {
		fmt.Println("✓ CA + ticket salt created")
	}

	var hosts []string
	for _, h := range strings.Split(prompt(in, "Receiver DNS name(s)/IP(s) forwarders connect to (comma-separated)", ""), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		fatalErr(fmt.Errorf("at least one hostname or IP is required"))
	}
	if err := pkiServer(dataDir, hosts); err != nil {
		fatalErr(err)
	}
	fmt.Println("✓ server cert written (cert.pem, key.pem)")

	port := prompt(in, "Ingest port", "8443")
	fmt.Println("\nDone. Start the receiver:")
	fmt.Printf("  PLV_INGEST_ADDR=:%s DATABASE_URL=postgres://... PLV_DATA_DIR=%s plv -addr :8080\n", port, dataDir)
	fmt.Println("\nEnrol a forwarder — run this on the receiver to mint its ticket:")
	fmt.Println("  plv pki ticket --cn <node-name>")
	fmt.Printf("\nGive each forwarder operator a copy of the CA cert (verifies this receiver):\n  %s\n", dataPath(dataDir, caCertFile))
}

func wizardForwarder(in *bufio.Reader, dataDir string) {
	to := prompt(in, "Receiver URL (https://host:port)", "")
	if to == "" {
		fatalErr(fmt.Errorf("receiver URL is required"))
	}
	caPath := prompt(in, "Path to the receiver's CA cert (ca.pem, copied from the receiver)", "")
	if caPath == "" {
		fatalErr(fmt.Errorf("the receiver CA certificate is required"))
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		fatalErr(fmt.Errorf("read CA file %q: %w", caPath, err))
	}

	cn := prompt(in, "This node's name (CN)", "")
	if cn == "" {
		fatalErr(fmt.Errorf("node name is required"))
	}
	ticket := prompt(in, "Enrolment ticket", "")
	if ticket == "" {
		fatalErr(fmt.Errorf("ticket is required"))
	}
	if err := enrollNode(dataDir, to, cn, ticket, caPEM); err != nil {
		fatalErr(err)
	}
	fmt.Println("\n✓ enrolled — wrote ca.pem, cert.pem, key.pem")
	fmt.Println("\nDone. Start the forwarder:")
	fmt.Printf("  PLV_FORWARD_TO=%s PLV_DATA_DIR=%s plv -logdir /var/log\n", to, dataDir)
}

func prompt(in *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := in.ReadString('\n')
	if line = strings.TrimSpace(line); line == "" {
		return def
	}
	return line
}
