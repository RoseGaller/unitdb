package main

import (
	"log"
	"time"

	"github.com/frontnet/tracedb"
)

func print(testdb *tracedb.DB) {
	it, err := testdb.Items(&tracedb.Query{Topic: []byte("dev18.b.b11?last=3m")})
	if err != nil {
		log.Fatal(err)
		return
	}
	for it.First(); it.Valid(); it.Next() {
		err := it.Error()
		if err != nil {
			log.Fatal(err)
			return
		}
		log.Printf("%s %s", it.Item().Key(), it.Item().Value())
	}
}

func main() {
	// Opening a database.
	testdb, err := tracedb.Open("example", nil)
	if err != nil {
		log.Fatal(err)
		return
	}
	defer testdb.Close()
	err = testdb.Batch(func(b *tracedb.Batch) error {
		opts := tracedb.DefaultBatchOptions
		opts.Encryption = true
		b.SetOptions(opts)
		b.Put([]byte("ttl.ttl1?ttl=3m"), []byte("bar"))
		b.Put([]byte("ttl.ttl2?ttl=3m"), []byte("bar"))
		b.Put([]byte("ttl.ttl3?ttl=3m"), []byte("bar"))
		err = b.Write()
		return err
	})
	if err != nil {
		log.Print(err)
	}
	print(testdb)

	g := testdb.NewBatchGroup()
	g.Add(func(b *tracedb.Batch, stop <-chan struct{}) error {
		b.Put([]byte("dev18.b1?ttl=2m"), []byte("bar"))
		b.Put([]byte("dev18.c1?ttl=1m"), []byte("bar"))
		b.Put([]byte("dev18.b1?ttl=3m"), []byte("bar2"))
		b.Write()
		go func() {
			<-stop // it signals batch group completion
			log.Printf("batch group completed")
		}()
		return nil
	})

	g.Add(func(b *tracedb.Batch, stop <-chan struct{}) error {
		b.Put([]byte("dev18.b.b11"), []byte("bar"))
		b.Put([]byte("dev18.b.b11"), []byte("bar2"))
		b.Put([]byte("dev18.b.b1"), []byte("bar3"))
		b.Put([]byte("dev18.c.c11"), []byte("bar"))
		b.Write()
		go func() {
			<-stop // it signals batch group completion
			log.Printf("batch group completed")
		}()
		return nil
	})

	g.Add(func(b *tracedb.Batch, stop <-chan struct{}) error {
		b.Put([]byte("dev18.b.b111"), []byte("bar"))
		b.Put([]byte("dev18.b.b111"), []byte("bar2"))
		b.Put([]byte("dev18.b.b11"), []byte("bar3"))
		b.Put([]byte("dev18.c.c111"), []byte("bar"))
		b.Write()
		go func() {
			<-stop // it signals batch group completion
			log.Printf("batch group completed")
		}()
		return nil
	})

	err = g.Run()

	if err != nil {
		log.Fatal(err)
		return
	}

	testdb.Batch(func(b *tracedb.Batch) error {
		b.Delete([]byte("dev18.b.b111"))
		err := b.Write()
		if err != nil {
			log.Printf("Error update1: %s", err)
		}
		return err
	})

	print(testdb)

	func(retry int) {
		i := 0
		for _ = range time.Tick(60 * time.Second) {
			err := testdb.Batch(func(b *tracedb.Batch) error {
				b.Put([]byte("dev18.b.b11?ttl=1m"), []byte("bar4"))
				err := b.Write()

				return err
			})
			if err != nil {
				log.Printf("Error update1: %s", err)
			}
			print(testdb)
			if i >= retry {
				return
			}
			i++
		}
	}(7)
}
