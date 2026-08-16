package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/compgenlab/varianthub-web/internal/blob"
	"github.com/compgenlab/varianthub-web/internal/config"
)

// Configuring the bucket rule that makes a download link usable in a browser.
//
// An operator command rather than something serve does at startup, for one
// reason: PutBucketCors replaces a bucket's whole CORS configuration. S3 has no
// "add a rule". A service that quietly rewrote it on every boot would erase a
// rule somebody else's application depends on, and it would do it on a bucket
// this deployment may only be a tenant of. So writing is asked for; reading is
// what serve does on its own, and what it acts on. See blob.VerifyPublicSites.

// cmdS3 dispatches the s3 subcommands.
func cmdS3(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: s3 cors [--apply]")
	}
	switch args[0] {
	case "cors":
		return cmdS3CORS(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("unknown s3 command %q; try: s3 cors", args[0])
	}
}

// cmdS3CORS reports, and with --apply sets, the CORS rules on each public site.
//
// Reporting by default. This writes to a bucket, so the version that changes
// something is the one you have to ask for by name.
func cmdS3CORS(ctx context.Context, cfg *config.Config, args []string) error {
	apply := false
	for _, a := range args {
		switch a {
		case "--apply", "-apply":
			apply = true
		default:
			return fmt.Errorf("unknown flag %q; usage: s3 cors [--apply]", a)
		}
	}

	origins := cfg.DownloadOrigins()
	if len(origins) == 0 {
		return fmt.Errorf("no web origin is configured, so there is nothing to " +
			"allow; set public_url (or cors_origins) first")
	}

	// Only the public ones. A site whose objects are always relayed through
	// this server is never fetched by a browser directly, so a CORS rule on it
	// would allow something that does not happen.
	var public []config.S3Site
	for _, s := range cfg.S3Sites {
		if s.PublicEndpoint {
			public = append(public, s)
		}
	}
	if len(public) == 0 {
		// The environment-variable arrangement, which declares no site at all:
		// AWS_ENDPOINT_URL plus VHW_S3_PUBLIC_ENDPOINT. This cannot check it —
		// there is no bucket named anywhere to go and ask about — and saying
		// nothing would read as "no rule needed", which is the opposite of true.
		if envPublicEndpoint() {
			fmt.Println("VHW_S3_PUBLIC_ENDPOINT is set, so download links are minted, but no")
			fmt.Println("[[s3]] block names a bucket — so neither this command nor the startup")
			fmt.Println("check can verify its CORS rules. Either declare the site in config, or")
			fmt.Println("set the bucket's rule by hand to allow GET from:")
			for _, o := range origins {
				fmt.Printf("  %s\n", o)
			}
			return nil
		}
		fmt.Println("No site sets public_endpoint, so no download links are minted and")
		fmt.Println("no bucket needs a CORS rule. Every export is relayed through this server.")
		return nil
	}

	fmt.Printf("Web origin(s) a download must be readable from: %s\n\n",
		strings.Join(origins, ", "))

	bad := 0
	for _, s := range public {
		site := blob.Site(s)
		if apply {
			if err := blob.PutCORS(ctx, site, origins); err != nil {
				bad++
				fmt.Printf("  %-20s could not set the rule: %v\n", s.Name, err)
				continue
			}
		}
		missing, known, err := blob.CheckCORS(ctx, site, origins)
		switch {
		case err != nil && !known:
			// Not counted as a failure. A key that can read and write objects
			// but not read the bucket's configuration is an ordinary least-
			// privilege setup, and the rule may well be right.
			fmt.Printf("  %-20s cannot be checked from here: %v\n", s.Name, err)
		case len(missing) > 0:
			bad++
			fmt.Printf("  %-20s does NOT allow GET from %s\n", s.Name, strings.Join(missing, ", "))
		default:
			fmt.Printf("  %-20s allows GET from the web origin\n", s.Name)
		}
	}

	if bad > 0 && !apply {
		fmt.Println()
		fmt.Println("Downloads from those sites will work from curl and fail in the browser.")
		fmt.Println("Run `varianthub-web s3 cors --apply` to write the rule.")
		return fmt.Errorf("%d site(s) need a CORS rule", bad)
	}
	if bad > 0 {
		return fmt.Errorf("%d site(s) still need a CORS rule", bad)
	}
	return nil
}

// envPublicEndpoint reports the environment-variable form of public_endpoint.
//
// Read here rather than through blob, which treats it as one input among
// several to a decision this command is not making.
func envPublicEndpoint() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("VHW_S3_PUBLIC_ENDPOINT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
