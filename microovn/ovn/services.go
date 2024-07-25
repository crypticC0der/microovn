package ovn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/canonical/microcluster/state"
	"github.com/canonical/microovn/microovn/database"
)

func DisableService(s *state.State, service string) error {
	err := s.Database.Transaction(s.Context, func(ctx context.Context, tx *sql.Tx) error {
		exists, err := database.ServiceExists(ctx, tx, s.Name(), service)
		if err != nil {
			return err
		}
		if exists != true {
			return errors.New("No such service")
		}
		return nil
	})
	if err != nil {
		return err
	}

	err = snapStop(service, true)
	if err != nil {
		return fmt.Errorf("Snapctl error, likely due to service not existing:\n %w", err)
	}

	err = s.Database.Transaction(s.Context, func(ctx context.Context, tx *sql.Tx) error {
		err := database.DeleteService(ctx, tx, s.Name(), service)
		return err
	})

	return err

}

func EnableService(s *state.State, service string) error {
	err := s.Database.Transaction(s.Context, func(ctx context.Context, tx *sql.Tx) error {
		exists, err := database.ServiceExists(ctx, tx, s.Name(), service)
		if err != nil {
			return err
		}
		if exists == true {
			return errors.New("Service already exists")
		}
		return nil
	})
	if err != nil {
		return err
	}

	err = snapStart(service, true)
	if err != nil {
		return fmt.Errorf("Snapctl error, likely due to service not existing:\n%w", err)
	}

	err = s.Database.Transaction(s.Context, func(ctx context.Context, tx *sql.Tx) error {
		_, err := database.CreateService(ctx, tx, database.Service{Member: s.Name(), Service: service})
		return err
	})

	return err

}
