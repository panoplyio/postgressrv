package protocol

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/jackc/pgx/pgproto3"
	pgstories "github.com/panoplyio/pg-stories"
	"github.com/stretchr/testify/require"
	"io"
	"net"
	"testing"
	"time"
)

func TestTransport_StartUp(t *testing.T) {
	t.Run("supported protocol version", func(t *testing.T) {
		buf := bytes.Buffer{}
		comm := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))
		transport := NewTransport(comm, comm)

		_, err := comm.Write([]byte{
			0, 0, 0, 8, // length
			0, 3, 0, 0, // 3.0
			0, 0, 0, 0,
		})
		require.NoError(t, err)

		err = comm.Flush()
		require.NoError(t, err)

		_, err = transport.StartUp()
		require.NoError(t, err)
		require.Equal(t, true, transport.initialized)
	})

	t.Run("unsupported protocol version", func(t *testing.T) {
		buf := bytes.Buffer{}
		comm := bufio.NewReadWriter(bufio.NewReader(&buf), bufio.NewWriter(&buf))
		transport := NewTransport(comm, comm)

		_, err := comm.Write([]byte{
			0, 0, 0, 8, // length
			0, 2, 0, 0, // 2.0
			0, 0, 0, 0,
		})
		require.NoError(t, err)

		err = comm.Flush()
		require.NoError(t, err)

		_, err = transport.StartUp()
		require.Error(t, err, "expected error of unsupported version. got none")
	})
}

func runStory(t *testing.T, conn io.ReadWriter, steps []pgstories.Step) error {
	frontend, err := pgproto3.NewFrontend(conn, conn)
	if err != nil {
		return err
	}

	story := &pgstories.Story{
		Steps:    steps,
		Frontend: frontend,
	}

	sigKill := make(chan interface{})
	timer := time.NewTimer(time.Second * 2)
	go func() {
		<-timer.C
		sigKill <- fmt.Errorf("timeout")
	}()

	err = story.Run(t, sigKill)
	if err != nil {
		timer.Stop()
	}
	return err
}

func TestTransport_Read(t *testing.T) {
	t.Run("standard message flow", func(t *testing.T) {
		f, b := net.Pipe()

		frontend, err := pgproto3.NewFrontend(f, f)
		require.NoError(t, err)

		transport := NewTransport(b, b)
		transport.initialized = true

		msg := make(chan pgproto3.FrontendMessage)
		go func() {
			m, err := transport.NextFrontendMessage()
			require.NoError(t, err)

			msg <- m
		}()

		m, err := frontend.Receive()
		require.NoError(t, err)
		require.IsType(t, &pgproto3.ReadyForQuery{}, m, "expected protocol to send ReadyForQuery message")

		err = frontend.Send(&pgproto3.Query{})
		require.NoError(t, err)

		res := <-msg

		require.IsTypef(t, &pgproto3.Query{}, res,
			"expected protocol to identify sent message as type %T. actual: %T", &pgproto3.Query{}, res)

		require.Nil(t, transport.transaction, "expected protocol not to start transaction")
	})

	t.Run("extended query message flow", func(t *testing.T) {
		t.Run("starts transaction", func(t *testing.T) {
			f, b := net.Pipe()

			transport := NewTransport(b, b)
			transport.initialized = true

			go func() {
				for {
					_, err := transport.NextFrontendMessage()
					require.NoError(t, err)
				}
			}()

			err := runStory(t, f, []pgstories.Step{
				&pgstories.Response{BackendMessage: &pgproto3.ReadyForQuery{}},
				&pgstories.Command{FrontendMessage: &pgproto3.Parse{}},
				&pgstories.Command{FrontendMessage: &pgproto3.Bind{}},
			})
			require.NoError(t, err)

			require.NotNil(t, transport.transaction, "expected protocol to start transaction")
		})

		t.Run("ends transaction", func(t *testing.T) {
			f, b := net.Pipe()

			transport := NewTransport(b, b)
			transport.initialized = true

			go func() {
				for {
					m, err := transport.NextFrontendMessage()
					require.NoError(t, err)

					err = nil
					switch m.(type) {
					case *pgproto3.Parse:
						err = transport.Write(ParseComplete)
					case *pgproto3.Bind:
						err = transport.Write(BindComplete)
					}
					require.NoError(t, err)
				}
			}()

			err := runStory(t, f, []pgstories.Step{
				&pgstories.Response{BackendMessage: &pgproto3.ReadyForQuery{}},
				&pgstories.Command{FrontendMessage: &pgproto3.Parse{}},
				&pgstories.Command{FrontendMessage: &pgproto3.Bind{}},
				&pgstories.Command{FrontendMessage: &pgproto3.Sync{}},
				&pgstories.Response{BackendMessage: &pgproto3.ParseComplete{}},
				&pgstories.Response{BackendMessage: &pgproto3.BindComplete{}},
				&pgstories.Response{BackendMessage: &pgproto3.ReadyForQuery{}},
			})

			require.NoError(t, err)

			require.Nil(t, transport.transaction, "expected protocol to end transaction")
		})
	})
}
