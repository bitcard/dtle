package mysql

import (
	"bytes"
	gosql "database/sql"
	"encoding/gob"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	gnatsd "github.com/nats-io/gnatsd/server"
	stan "github.com/nats-io/go-nats-streaming"
	stand "github.com/nats-io/nats-streaming-server/server"
	"github.com/ngaut/log"

	uconf "udup/config"
	ubinlog "udup/plugins/mysql/binlog"
	usql "udup/plugins/mysql/sql"
)

var (
	waitTime    = 10 * time.Millisecond
	maxWaitTime = 3 * time.Second
)

type Applier struct {
	cfg         *uconf.DriverConfig
	dbs         []*gosql.DB
	singletonDB *gosql.DB
	eventChans  []chan usql.StreamEvent

	eventsChannel chan *ubinlog.BinlogEvent
	currentTx     *ubinlog.Transaction_t
	txChan        chan *ubinlog.Transaction_t

	stanConn stan.Conn
	stanSub  stan.Subscription
	stand    *stand.StanServer
	gnatsd   *gnatsd.Server
}

func NewApplier(cfg *uconf.DriverConfig) *Applier {
	return &Applier{
		cfg:        cfg,
		eventChans: newEventChans(cfg.WorkerCount),
		txChan:     make(chan *ubinlog.Transaction_t, 100),
	}
}

func newEventChans(count int) []chan usql.StreamEvent {
	events := make([]chan usql.StreamEvent, 0, count)
	for i := 0; i < count; i++ {
		events = append(events, make(chan usql.StreamEvent, 1000))
	}

	return events
}

func (a *Applier) InitiateApplier() error {
	log.Infof("Apply binlog events onto the datasource :%v", a.cfg.ConnCfg)
	if err := a.setupNatsServer(); err != nil {
		return err
	}

	if err := a.initDBConnections(); err != nil {
		return err
	}

	if err := a.initNatSubClient(); err != nil {
		return err
	}

	if err := a.initiateStreaming(); err != nil {
		return err
	}

	// start N applier worker
	for i := 0; i < a.cfg.WorkerCount; i++ {
		go a.startApplierWorker(i, a.dbs[i])
	}

	return nil
}

func (a *Applier) onError(err error) {
	a.cfg.ErrCh <- err
}

func (a *Applier) applyTx(db *gosql.DB, transaction *ubinlog.Transaction_t) error {
	if len(transaction.Query) == 0 {
		return nil
	}

	var lastFde, lastGtid string
	if lastFde != transaction.Fde {
		lastFde = transaction.Fde // IMO it would comare the internal pointer first
		_, err := db.Exec(lastFde)
		if err != nil {
			log.Errorf("err applying fde::%v\n", err)
			//a.onError(err)
			return err
		}
	}

	if lastGtid != transaction.Gtid {
		lastGtid = transaction.Gtid // IMO it would comare the internal pointer first
		_, err := db.Exec(fmt.Sprintf(`SET GTID_NEXT='%v'`, lastGtid))
		if err != nil {
			log.Errorf("err applying gtid::%v\n", err)
			//a.onError(err)
			return err
		}

		txn, err := db.Begin()
		if err != nil {
			return err
		}

		err = txn.Commit()
		if err != nil {
			return err
		}

		if _, err := db.Exec(`SET GTID_NEXT='AUTOMATIC'`); err != nil {
			return err
		}
		a.cfg.GtidCh <- lastGtid
	}

	tx, err := db.Begin()
	if err != nil {
		log.Infof("directlyApplyBinlog Begin err:%v", err)
		return err
	}
	for _, query := range transaction.Query {
		if query == "" {
			return nil
		}

		_, err = tx.Exec(query)
		if err != nil {
			log.Errorf("err:%v", err)
			return err
		}
	}

	return tx.Commit()
}

func (a *Applier) startApplierWorker(i int, db *gosql.DB) {
	for {
		var tx *ubinlog.Transaction_t
		select {
		case tx = <-a.txChan:
			err := a.applyTx(db, tx)
			if err != nil {
				log.Errorf("err applying tx: %v\n", err)
				//a.onError(err)
				a.cfg.ErrCh <- err
			}

			//__log.Debug("release %v", node.tx.eventSize)
			atomic.AddInt64(&a.cfg.MemoryLimit, int64(tx.EventSize))
		default:
			/*t1 := time.Now().UnixNano()
				select {
				case tx = <-a.ds.txChan:
				case <-time.After(500 * time.Millisecond):
					a.ds.commit()
					idle_ns += 500 * 1000000
					continue
				}
			t2 := time.Now().UnixNano()
			idle_ns += (t2 - t1)*/
		}
		/*node := <-chNode
		node.inDegreeWg.Wait()
		tx := node.tx*/

		//node.releaseSuccs()
		//a.wgAllNodes.Done()
	}
}

/*func (a *Applier) onError(err error) {
	a.cfg.ErrCh <- err
}*/

func (a *Applier) initNatSubClient() (err error) {
	sc, err := stan.Connect("test-cluster", "sub1", stan.NatsURL(fmt.Sprintf("nats://%s", a.cfg.NatsAddr)))
	if err != nil {
		log.Fatalf("Can't connect: %v.\nMake sure a NATS Streaming Server is running at: %s", err, fmt.Sprintf("nats://%s", a.cfg.NatsAddr))
	}
	a.stanConn = sc
	return nil
}

// Decode
func Decode(data []byte, vPtr interface{}) (err error) {
	dec := gob.NewDecoder(bytes.NewBuffer(data))
	err = dec.Decode(vPtr)
	return
}

// initiateStreaming begins treaming of binary log events and registers listeners for such events
func (a *Applier) initiateStreaming() error {
	sub, err := a.stanConn.Subscribe("subject", func(m *stan.Msg) {
		tx := &ubinlog.Transaction_t{}
		if err := Decode(m.Data, tx); err != nil {
			log.Infof("Subscribe err:%v", err)
			a.cfg.ErrCh <- err
		}
		/*idx := int(usql.GenHashKey(event.Key)) % a.cfg.WorkerCount
		a.eventChans[idx] <- event*/
		a.txChan <- tx
	})

	if err != nil {
		log.Errorf("Unexpected error on Subscribe, got %v", err)
		return err
	}
	a.stanSub = sub
	return nil
}

func (a *Applier) setupNatsServer() error {
	host, port, err := net.SplitHostPort(a.cfg.NatsAddr)
	p, err := strconv.Atoi(port)
	if err != nil {
		return err
	}

	nOpts := gnatsd.Options{
		Host:       host,
		Port:       p,
		MaxPayload: (100 * 1024 * 1024),
		Trace:      true,
		Debug:      true,
	}
	gnats := gnatsd.New(&nOpts)
	go gnats.Start()
	// Wait for accept loop(s) to be started
	if !gnats.ReadyForConnections(10 * time.Second) {
		return fmt.Errorf("Unable to start NATS Server in Go Routine")
	}
	a.gnatsd = gnats
	sOpts := stand.GetDefaultOptions()
	if a.cfg.StoreType == "FILE" {
		sOpts.StoreType = a.cfg.StoreType
		sOpts.FilestoreDir = a.cfg.FilestoreDir
	}
	sOpts.NATSServerURL = fmt.Sprintf("nats://%s", a.cfg.NatsAddr)
	s := stand.RunServerWithOpts(sOpts, nil)
	a.stand = s
	return nil
}

func (a *Applier) initDBConnections() (err error) {
	if a.singletonDB, err = usql.CreateDB(a.cfg.ConnCfg); err != nil {
		return err
	}
	a.singletonDB.SetMaxOpenConns(1)
	if err := a.mysqlGTIDMode(); err != nil {
		return err
	}

	if a.dbs, err = usql.CreateDBs(a.cfg.ConnCfg, a.cfg.WorkerCount+1); err != nil {
		return err
	}
	return nil
}

func (a *Applier) mysqlGTIDMode() error {
	query := `SELECT @@gtid_mode`
	var gtidMode string
	if err := a.singletonDB.QueryRow(query).Scan(&gtidMode); err != nil {
		return err
	}
	if gtidMode != "ON" {
		return fmt.Errorf("must have GTID enabled: %+v", gtidMode)
	}
	return nil
}

func closeEventChans(events []chan usql.StreamEvent) {
	for _, ch := range events {
		close(ch)
	}
}

func (a *Applier) Shutdown() error {
	usql.CloseDBs(a.dbs...)

	closeEventChans(a.eventChans)

	a.stanSub.Unsubscribe()
	a.stanConn.Close()
	a.stand.Shutdown()
	a.gnatsd.Shutdown()
	return nil
}
