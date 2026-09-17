package exclude

// DefaultAMPExclusions are applied to every AMP instance unless explicitly
// disabled. They exist because the obvious first run of a backup tool against
// an AMP instance would otherwise back up AMP's own backups: the Backups
// directory of a busy instance can hold tens of gigabytes of full ZIP archives,
// which is both useless (the data is already in the snapshot, uncompressed) and
// large enough to fill the disk.
//
// Everything listed here is either regenerable, a log, or a lock. Nothing that
// a restored instance needs to boot is in this list.
var DefaultAMPExclusions = []string{
	// AMP's own full-ZIP backups. This is the entry that matters most.
	"Backups",

	// AMP's own logs and caches; the panel regenerates them.
	"AMP_Logs",
	"AMPLogs",
	"ModpacksCache.json",
	"ForgeVersionManifest.xml",
	"NeoForgeVersionManifest.xml",
	"geoip-data.mmdb.gz",

	// Application logs and crash artefacts.
	"logs",
	"crash-reports",
	"**/*.log.gz",
	"hs_err_pid*.log",
	// Narrowed to a numeric suffix on purpose: a bare "core.*" would also
	// match config files such as bluemap's core.conf.
	"core.[0-9]*",
	"replay_pid*.log",

	// AMP's file-manager trash. Deleting a file through the panel moves it
	// here rather than removing it, so on a long-lived instance this quietly
	// accumulates gigabytes of things the operator already threw away.
	"**/.trash",

	// Locks and scratch state that must never be restored.
	"**/session.lock",
	"**/*.lock",
	"**/*.pid",
	"**/.fabric",

	// Rendered map tiles: large, rewritten constantly, and fully derivable
	// from the world data that is being backed up anyway.
	"**/bluemap/web",
	"**/dynmap/web",
	"**/squaremap/web",

	// Anything this tool itself produced.
	"**/.amp-bb-restore-*",
}

// DefaultProfile returns the built-in exclusions as a compiled set.
func DefaultProfile() *Set { return MustCompile(DefaultAMPExclusions) }

// DefaultHotPatterns are the paths Minecraft rewrites in place. Only these are
// read while the server is quiesced; everything else is read with the server
// running normally, which is what keeps the quiesce window down to the delta
// rather than the whole instance.
//
// It lives here rather than in the CLI because the schedule the daemon runs on
// and the flag default a person types must be the same list. Two lists that
// drift apart would mean a backup taken from the browser quiescing different
// files than one taken from the command line.
var DefaultHotPatterns = []string{
	"**/world*/**",
	"**/*_world/**",
	"**/level.dat*",
	"**/playerdata/**",
	"**/*.mca",
	"**/*.mcr",
}

// DefaultHotProfile returns the built-in hot paths as a compiled set.
func DefaultHotProfile() *Set { return MustCompile(DefaultHotPatterns) }
