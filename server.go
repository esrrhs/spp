package main

type Server struct {
	key int
}

func NewServer(key int) (*Server, error) {
	s := &Server{
		key: key,
	}
	return s, nil
}

func (s *Server) Run() error {

}
