package docwriter

import (
	"strings"
	"sync"
	"time"

	"github.com/bglebrun/rita/config"
	"github.com/bglebrun/rita/database"

	log "github.com/Sirupsen/logrus"
	"github.com/davecgh/go-spew/spew"
	"gopkg.in/mgo.v2"
)

type (
	// Document holds one item to be written to a database
	Document struct {
		Doc  interface{} // Thing to write
		DB   string      // DB to write to
		Coll string      // Collection to write to
	}

	// DocWriter writes documents to a database
	DocWriter struct {
		Ssn       *mgo.Session           // Session to db instance
		pre       string                 // Prefix to the database
		ImportWl  bool                   // Flag to import whitelist
		Whitelist []string               // Pointer to our whitelist array
		wchan     chan Document          // Document channel
		log       *log.Logger            // Logging
		wg        *sync.WaitGroup        // Used to block until complete
		Meta      *database.MetaDBHandle // Handle to metadata
		Databases []string               // Track the db states
		started   bool                   // Track if we've started the writer
		dblock    *sync.Mutex            // For the Databases fields
	}
)

// New generates a new DocWriter
func New(cfg *config.Resources, mdb *database.MetaDBHandle) *DocWriter {

	dbs := mdb.GetDatabases()
	return &DocWriter{
		Ssn:       cfg.Session.Copy(),
		log:       cfg.Log,
		pre:       cfg.System.BroConfig.DBPrefix,
		ImportWl:  cfg.System.ImportWhitelist,
		Whitelist: cfg.System.Whitelist,
		wchan:     make(chan Document, 5000),
		wg:        new(sync.WaitGroup),
		Meta:      mdb,
		Databases: dbs,
		started:   false,
		dblock:    new(sync.Mutex)}
}

// Start begins the DocWriter spinning on its input
func (d *DocWriter) Start(count int) {
	// Add a second layer of protection against untracked starts.
	if !d.started {
		for i := 0; i < count; i++ {
			d.started = true
			go d.writeLoop()
		}
	}
	return
}

// IsStarted checks to see if the writer is already going
func (d *DocWriter) IsStarted() bool {
	return d.started
}

// Write allows a user to add to the channel
func (d *DocWriter) Write(doc Document) {
	doc.DB = d.pre + doc.DB
	seen := false
	d.dblock.Lock()
	for _, aval := range d.Databases {
		if aval == doc.DB {
			seen = true
		}
	}

	if !seen {
		d.Meta.AddNewDB(doc.DB)
		d.Databases = append(d.Databases, doc.DB)
	}
	d.dblock.Unlock()
	d.wchan <- doc
	return
}

// Flush writes the final documents to the db and exits docwriter
func (d *DocWriter) Flush() {
	d.log.Debug("closing write channel")
	close(d.wchan)
	d.log.Debug("waiting for writes to finish")
	d.wg.Wait()
	d.log.Debug("writes completed, exiting write loop")
	return
}

/*
 * Ben L.
 * Checks if our document is present in the whitelist
 * and returns true if the string is whitelisted, false
 * otherwise
 */
func isWhitelisted(whitelist []string, url string) bool {
	if whitelist == nil {
		return false
	}
	for count := range whitelist {
		if strings.Contains(url, whitelist[count]) {
			return true
		}
	}
	return false
}

// writeLoop loops over the input channel spawning threads to write
// TODO: implement whitelist code here, pass config somehow
func (d *DocWriter) writeLoop() {
	var err error
	d.wg.Add(1)
	for {
		d.log.WithFields(log.Fields{
			"type":             "wldebug",
			"write_chan_count": len(d.wchan),
		}).Debug("WriteLoop status")
		doc, ok := <-d.wchan
		if !ok {
			d.log.Info("WriteLoop got closed channel, exiting")
			break
		}
		// Right here is where we check for our "import whitelist"
		// option before proceeding for anything
		ssn := d.Ssn.Copy()
		// This is where we check for our whitelist!!!!
		towrite := doc.Doc
		// Find a way to grab our host name, original implementation sucked
		if isWhitelisted(d.Whitelist, "This is a cool wholesome string") {
			if d.ImportWl {
				err = ssn.DB(doc.DB).C(doc.Coll).Insert(towrite)
			}
		} else {
			err = ssn.DB(doc.DB).C(doc.Coll).Insert(towrite)
		}
		if err != nil {
			if strings.Contains(err.Error(), "ObjectIDs") {
				spew.Dump(towrite)
			}
			d.log.WithFields(log.Fields{
				"error": err.Error(),
			}).Error("Database write failure")

			d.expFalloff(&doc)
		}
		ssn.Close()
	}

	d.Ssn.Close()
	d.wg.Done()
	return
}

// expFalloff is entered after dbwrite failure
func (d *DocWriter) expFalloff(doc *Document) {
	for i := 0; i < 5; i++ {
		time.Sleep(time.Duration(i*i) * time.Second)
		ssn := d.Ssn.Copy()
		towrite := doc.Doc
		err := ssn.DB(doc.DB).C(doc.Coll).Insert(towrite)
		if err == nil {
			ssn.Close()
			d.log.Info("Write succeeded")
			return
		}
		ssn.Close()
		d.log.WithFields(log.Fields{
			"error":   err.Error(),
			"falloff": i,
		}).Error("Database write failure")

	}
}
