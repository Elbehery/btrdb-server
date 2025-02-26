// Copyright (c) 2021 Michael Andersen
// Copyright (c) 2021 Regents of the University Of California
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file or at
// https://opensource.org/licenses/MIT.

// +build ignore

package fileprovider

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/BTrDB/btrdb-server/bte"
	"github.com/BTrDB/btrdb-server/internal/bprovider"
	"github.com/BTrDB/btrdb-server/internal/configprovider"
	"github.com/op/go-logging"
)

var log *logging.Logger

func init() {
	log = logging.MustGetLogger("log")
}

const NUMFILES = 256

type writeparams struct {
	Address uint64
	Data    []byte
}

type FileProviderSegment struct {
	sp    *FileStorageProvider
	fidx  int
	f     *os.File
	base  int64
	ptr   int64
	wchan chan writeparams
	wg    sync.WaitGroup
}

type FileStorageProvider struct {
	fidx     chan int
	retfidx  chan int
	dbf      []*os.File
	dbrf     []*os.File
	dbrf_mtx []sync.Mutex
	favail   []bool
}

func (seg *FileProviderSegment) writer() {

	for args := range seg.wchan {
		off := int64(args.Address & ((1 << 50) - 1))
		lenarr := make([]byte, 2)
		lenarr[0] = byte(len(args.Data))
		lenarr[1] = byte(len(args.Data) >> 8)
		_, err := seg.f.WriteAt(lenarr, off)
		if err != nil {
			log.Panic("File writing error %v", err)
		}
		_, err = seg.f.WriteAt(args.Data, off+2)
		if err != nil {
			log.Panic("File writing error %v", err)
		}
	}
	seg.wg.Done()
}
func (seg *FileProviderSegment) init() {
	seg.wchan = make(chan writeparams, 16)
	seg.wg.Add(1)
	go seg.writer()
}

//Returns the address of the first free word in the segment when it was locked
func (seg *FileProviderSegment) BaseAddress() uint64 {
	//This seems arbitrary, why not go with the top 8 bits? The reason is this:
	//a) this still leaves 1PB per file
	//b) The huffman encoding can do 58 bits in 8 bytes, but anything more is 9
	//c) if we later decide to more than 256 files, we can
	return (uint64(seg.fidx) << 50) + uint64(seg.base)
}

//Unlocks the segment for the StorageProvider to give to other consumers
//Implies a flush
func (seg *FileProviderSegment) Unlock() {
	seg.Flush()
	seg.sp.retfidx <- seg.fidx
}

//Writes a slice to the segment, returns immediately
//Returns nil if op is OK, otherwise ErrNoSpace or ErrInvalidArgument
//It is up to the implementer to work out how to report no space immediately
//The uint64 rv is the address to be used for the next write
func (seg *FileProviderSegment) Write(uuid []byte, address uint64, data []byte) (uint64, error) {
	//TODO remove
	if seg.ptr != int64(address&((1<<50)-1)) {
		log.Panic("Pointer does not match address %x vs %x", seg.ptr, int64(address&((1<<50)-1)))
	}
	wp := writeparams{Address: address, Data: data}
	seg.wchan <- wp
	seg.ptr = int64(address&((1<<50)-1)) + int64(len(data)) + 2
	return uint64(seg.ptr) + (uint64(seg.fidx) << 50), nil
}

//Block until all writes are complete, not
func (seg *FileProviderSegment) Flush() {
	close(seg.wchan)
	seg.wg.Wait()
}

//Provide file indices into fidx, does not return
func (sp *FileStorageProvider) provideFiles() {
	for {
		//Read all returned files
	ldretfi:
		for {
			select {
			case fi := <-sp.retfidx:
				sp.favail[fi] = true
			default:
				break ldretfi
			}
		}

		//Greedily select file
		minidx := -1
		var minv int64 = 0
		for i := 0; i < NUMFILES; i++ {
			if !sp.favail[i] {
				continue
			}
			off, err := sp.dbf[i].Seek(0, os.SEEK_CUR)
			if err != nil {
				log.Panic(err)
			}
			if minidx == -1 || off < minv {
				minidx = i
				minv = off
			}
		}

		//Return it, or do blocking read if not found
		if minidx != -1 {
			sp.favail[minidx] = false
			sp.fidx <- minidx
		} else {
			//Do a blocking read on retfidx to avoid fast spin on nonblocking
			fi := <-sp.retfidx
			sp.favail[fi] = true
		}

	}
}

//Called at startup
func (sp *FileStorageProvider) Initialize(cfg configprovider.Configuration) {
	//Initialize file indices thingy
	sp.fidx = make(chan int)
	sp.retfidx = make(chan int, NUMFILES+1)
	sp.dbf = make([]*os.File, NUMFILES)
	sp.dbrf = make([]*os.File, NUMFILES)
	sp.dbrf_mtx = make([]sync.Mutex, NUMFILES)
	sp.favail = make([]bool, NUMFILES)
	for i := 0; i < NUMFILES; i++ {
		//Open file
		dbpath := cfg.StorageFilepath()
		fname := fmt.Sprintf("%s/blockstore.%02x.db", dbpath, i)
		//write file descriptor
		{
			f, err := os.OpenFile(fname, os.O_RDWR, 0666)
			if err != nil && os.IsNotExist(err) {
				log.Critical("Aborting: seems database does not exist. Have you run `btrdbd -makedb`?")
				os.Exit(1)
			}
			if err != nil {
				log.Panicf("Problem with blockstore DB: ", err)
			}
			sp.dbf[i] = f
		}
		//Read file descriptor
		{
			f, err := os.OpenFile(fname, os.O_RDONLY, 0666)
			if err != nil {
				log.Panicf("Problem with blockstore DB: ", err)
			}
			sp.dbrf[i] = f
		}
		sp.favail[i] = true
	}
	go sp.provideFiles()

}

// Lock a segment, or block until a segment can be locked
// Returns a Segment struct
func (sp *FileStorageProvider) LockSegment(uuid []byte) bprovider.Segment {
	//Grab a file index
	fidx := <-sp.fidx
	f := sp.dbf[fidx]
	l, err := f.Seek(0, os.SEEK_END)
	if err != nil {
		log.Panicf("Error on lock segment: %v", err)
	}

	//Construct segment
	seg := &FileProviderSegment{sp: sp, fidx: fidx, f: sp.dbf[fidx], base: l, ptr: l}
	seg.init()

	return seg
}

//This is the size of a maximal size cblock + header
const FIRSTREAD = 3459

func (sp *FileStorageProvider) Read(uuid []byte, address uint64, buffer []byte) []byte {
	fidx := address >> 50
	off := int64(address & ((1 << 50) - 1))
	if fidx > NUMFILES {
		log.Panic("Encoded file idx too large")
	}
	sp.dbrf_mtx[fidx].Lock()
	nread, err := sp.dbrf[fidx].ReadAt(buffer[:FIRSTREAD], off)
	if err != nil && err != io.EOF {
		log.Panic("Non EOF read error: %v", err)
	}
	if nread < 2 {
		log.Panic("Unexpected (very) short read")
	}
	//Now we read the blob size
	bsize := int(buffer[0]) + (int(buffer[1]) << 8)
	if bsize > nread-2 {
		_, err := sp.dbrf[fidx].ReadAt(buffer[nread:bsize+2], off+int64(nread))
		if err != nil {
			log.Panic("Read error: %v", err)
		}
	}
	sp.dbrf_mtx[fidx].Unlock()
	return buffer[2 : bsize+2]
}

//Called to create the database for the first time
func (sp *FileStorageProvider) CreateDatabase(cfg configprovider.Configuration) error {
	for i := 0; i < NUMFILES; i++ {
		//Open file
		dbpath := cfg.StorageFilepath()
		fname := fmt.Sprintf("%s/blockstore.%02x.db", dbpath, i)
		//write file descriptor
		{
			f, err := os.OpenFile(fname, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0666)
			if err != nil && !os.IsExist(err) {
				log.Panicf("Problem with blockstore DB: ", err)
			} else if os.IsExist(err) {
				return bprovider.ErrExists
			}
			//Add a file tag
			//An exercise left for the reader: if you remove this, everything breaks :-)
			//Hint: what is the physical address of the first byte of file zero?
			_, err = f.Write([]byte("QUASARDB"))
			if err != nil {
				log.Panicf("Could not write to blockstore:", err)
			}

			err = f.Close()
			if err != nil {
				log.Panicf("Error on close %v", err)
			}
		}
	}
	return nil
}

// Read the given version of superblock into the buffer.
func (sp *FileStorageProvider) ReadSuperBlock(uuid []byte, version uint64, buffer []byte) []byte {
	panic("yo not supported bro")
}

// Writes a superblock of the given version
// TODO I think the storage will need to chunk this, because sb logs of gigabytes are possible
func (sp *FileStorageProvider) WriteSuperBlock(uuid []byte, version uint64, buffer []byte) {
	panic("yo not supported bro")
}

// Sets the version of a stream. If it is in the past, it is essentially a rollback,
// and although no space is freed, the consecutive version numbers can be reused
// note to self: you must make sure not to call ReadSuperBlock on versions higher
// than you get from GetStreamVersion because they might succeed
func (sp *FileStorageProvider) SetStreamVersion(uuid []byte, version uint64) {
	panic("yo not supported bro")
}

// Gets the version of a stream. Returns 0 if none exists.
func (sp *FileStorageProvider) GetStreamInfo(uuid []byte) (bprovider.Stream, uint64) {
	panic("yo not supported bro")
}

// Gets the version of a stream. Returns 0 if none exists.
func (sp *FileStorageProvider) GetStreamVersion(uuid []byte) uint64 {
	panic("yo not supported bro")
}

// CreateStream makes a stream with the given uuid, collection and tags. Returns
// an error if the uuid already exists.
func (sp *FileStorageProvider) CreateStream(uuid []byte, collection string, tags map[string]string, annotation []byte) bte.BTE {
	panic("yo not supported bro")
}

// ListCollections returns a list of collections beginning with prefix (which may be "")
// and starting from the given string. If number is > 0, only that many results
// will be returned. More can be obtained by re-calling ListCollections with
// a given startingFrom and number.
func (sp *FileStorageProvider) ListCollections(prefix string, startingFrom string, number int64) ([]string, bte.BTE) {
	panic("yo not supported bro")
}

// ListStreams lists all the streams within a collection. If tags are specified
// then streams are only returned if they have that tag, and the value equals
// the value passed. If partial is false, zero or one streams will be returned.
func (sp *FileStorageProvider) ListStreams(collection string, partial bool, tags map[string]string) ([]bprovider.Stream, bte.BTE) {
	panic("yo not supported bro")
}

// Sets the stream annotation
func (sp *FileStorageProvider) SetStreamAnnotation(uuid []byte, aver uint64, content []byte) bte.BTE {
	panic("yo not supported bro")
}

// Gets the stream annotation
func (sp *FileStorageProvider) GetStreamAnnotation(uuid []byte) ([]byte, uint64, bte.BTE) {
	panic("yo not supported bro")
}
