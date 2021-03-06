package broker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lib/pq"

	"github.com/golang/glog"
	_ "github.com/lib/pq"
	osb "github.com/pmorie/go-open-service-broker-client/v2"
)

/*
TODO: Add logging, levels 3 and 4
*/

const plansQuery string = `
select
    plans.plan,
    plans.service,
    services.name as service_name,
    plans.name,
    plans.human_name,
    plans.description,
    plans.version,
    plans.type,
    plans.scheme,
    plans.categories,
    plans.cost_cents,
    plans.cost_unit,
    plans.attributes::text,
    plans.installable_inside_private_network,
    plans.installable_outside_private_network,
    plans.supports_multiple_installations,
    plans.supports_sharing,
    plans.preprovision,
    plans.beta,
    plans.provider,
    plans.provider_private_details::text,
    plans.deprecated
from plans join services on services.service = plans.service
    where services.deleted = false and plans.deleted = false `

const servicesQuery string = `
select
    service,
    name,
    human_name,
    description,
    categories,
    image,
    beta,
    deprecated
from services where deleted = false `

const sqlCreateScript string = `
DO 
$$
begin
    create extension if not exists "uuid-ossp";

    if not exists (select 1 from pg_type where typname = 'alpha_numeric') then
        create domain alpha_numeric as varchar(128) check (value ~ '^[A-z0-9\-]+$');
    end if;

    if not exists (select 1 from pg_type where typname = 'enginetype') then
        create type enginetype as enum('mongodb');
    end if;

    if not exists (select 1 from pg_type where typname = 'clientdbtype') then
        create type clientdbtype as enum('mongodb', 'https');
    end if;

    if not exists (select 1 from pg_type where typname = 'cents') then
        create domain cents as int check (value >= 0);
    end if;

    if not exists (select 1 from pg_type where typname = 'costunit') then
        create type costunit as enum('year', 'month', 'day', 'hour', 'minute', 'second', 'cycle', 'byte', 'megabyte', 'gigabyte', 'terabyte', 'petabyte', 'op', 'unit');
    end if;

    if not exists (select 1 from pg_type where typname = 'task_status') then
        create type task_status as enum('pending', 'started', 'finished', 'failed');
    end if;

    create or replace function mark_updated_column() returns trigger as $emp_stamp$
    begin
        NEW.updated = now();
        return NEW;
    end;
    $emp_stamp$ language plpgsql;

    create table if not exists services
    (
        service uuid not null primary key,
        name alpha_numeric not null,
        human_name text not null,
        description text not null,
        categories varchar(1024) not null default 'Data Stores',
        image text not null default '',

        beta boolean not null default false,
        deprecated boolean not null default false,
        deleted boolean not null default false,
        
        created timestamp with time zone not null default now(),
        updated timestamp with time zone not null default now()
    );
    drop trigger if exists services_updated on services;
    create trigger services_updated before update on services for each row execute procedure mark_updated_column();

    create table if not exists plans
    (
        plan uuid not null primary key,
        service uuid references services("service") not null,
        name alpha_numeric not null,
        human_name text not null,
        description text not null,
        version text not null,
        type enginetype not null,
        scheme clientdbtype not null,
        categories text not null default '',
        cost_cents cents not null default 0,
        cost_unit costunit not null default 'month',
        attributes json not null,

        provider varchar(1024) not null,
        provider_private_details json not null,

        installable_inside_private_network bool not null default true,
        installable_outside_private_network bool not null default true,
        supports_multiple_installations bool not null default true,
        supports_sharing bool not null default true,
        preprovision int not null default 0,

        beta boolean not null default false,
        deprecated boolean not null default false,
        deleted boolean not null default false,
        
        created timestamp with time zone not null default now(),
        updated timestamp with time zone not null default now()
    );
    drop trigger if exists plans_updated on plans;
    create trigger plans_updated before update on plans for each row execute procedure mark_updated_column();

    create table if not exists resources
    (
        id varchar(1024) not null primary key,
        name varchar(200) not null,
        plan uuid references plans("plan") not null,
        claimed boolean not null default false,
        status varchar(1024) not null default 'unknown',
        username varchar(128),
        password varchar(128),
        endpoint varchar(128),
        created timestamp with time zone not null default now(),
        updated timestamp with time zone not null default now(),
        deleted bool not null default false
    );
    drop trigger if exists resources_updated on resources;
    create trigger resources_updated before update on resources for each row execute procedure mark_updated_column();

    create table if not exists tasks
    (
        task uuid not null primary key,
        resource varchar(1024) references resources("id") not null,
        action varchar(1024) not null,
        status task_status not null default 'pending',
        retries int not null default 0,
        metadata text not null default '',
        result text not null default '',
        created timestamp with time zone not null default now(),
        updated timestamp with time zone not null default now(),
        started timestamp with time zone,
        finished timestamp with time zone,
        deleted bool not null default false
    );
    
    if exists (SELECT NULL 
              FROM INFORMATION_SCHEMA.COLUMNS
             WHERE table_name = 'tasks'
              AND column_name = 'action'
              and udt_name = 'task_action'
              and table_schema = 'public') then
        alter table tasks alter column action TYPE varchar(1024) using action::varchar(1024);
    end if;

    if exists (SELECT NULL 
              FROM INFORMATION_SCHEMA.COLUMNS
             WHERE table_name = 'plans'
              AND column_name = 'provider'
              and udt_name = 'providertype'
              and table_schema = 'public') then
        alter table plans alter column provider TYPE varchar(1024) using provider::varchar(1024);
    end if;

    drop trigger if exists tasks_updated on tasks;
    create trigger tasks_updated before update on tasks for each row execute procedure mark_updated_column();

    -- populate some default services
    if (select count(*) from services) = 0 then
        insert into services 
            (service, name, human_name, description, categories, image, beta, deprecated)
        values 
			('1f1b58ff-8bbc-49f9-a0e4-708a11ec174a','akkeris-mongodb', 'Akkeris Mongodb', 'NOSQL database.', 'Data Stores,mongodb', '', false, false);
    end if;

    -- populate some default plans
    if (select count(*) from plans) = 0 then
        insert into plans 
            (plan, service, name, human_name, description, version, type, scheme, categories, cost_cents, preprovision, attributes, provider, provider_private_details)
        values 
            ('54f201a4-b4be-4f29-87b3-25f68ef2c8f2', '1f1b58ff-8bbc-49f9-a0e4-708a11ec174a', 'shared', 'Shared', 'Mongodb shared instance', '3.6.3', 'mongodb', 'mongodb', 'Data Stores', 2500, 0, '{}', 'mongodb', '{"master_uri":"${MONGODB_SHARED_URI}", "engine":"mongodb", "engine_version":"3.2"}');
    end if;
end
$$
`

func cancelOnInterrupt(ctx context.Context, db *sql.DB) {
	term := make(chan os.Signal)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-term:
			db.Close()
		case <-ctx.Done():
			db.Close()
		}
	}
}

type Storage interface {
	GetPlans(string) ([]ProviderPlan, error)
	GetPlanByID(string) (*ProviderPlan, error)
	GetInstance(string) (*Entry, error)
	AddInstance(*Instance) error
	DeleteInstance(*Instance) error
	UpdateInstance(*Instance, string) error
	AddTask(string, TaskAction, string) (string, error)
	GetServices() ([]osb.Service, error)
	UpdateTask(string, *string, *int64, *string, *string, *time.Time, *time.Time) error
	PopPendingTask() (*Task, error)
	GetUnclaimedInstance(string, string) (*Entry, error)
	ReturnClaimedInstance(string) error
	StartProvisioningTasks() ([]Entry, error)
	NukeInstance(string) error
	WarnOnUnfinishedTasks()
	IsRestoring(string) (bool, error)
	IsUpgrading(string) (bool, error)
	ValidateInstanceID(string) error
}

type PostgresStorage struct {
	Storage
	db *sql.DB
}

func (b *PostgresStorage) getPlans(subquery string, arg string) ([]ProviderPlan, error) {

	glog.V(3).Infoln("[getPlans] start")

	// arg could be a service ID or Plan Id
	rows, err := b.db.Query(plansQuery+subquery, arg)
	if err != nil {
		glog.Errorf("GetPlans query failed: %s\n", err.Error())
		return nil, err
	}
	defer rows.Close()
	plans := make([]ProviderPlan, 0)
	for rows.Next() {
		var planId, serviceId, serviceName, name, humanName, description, engineVersion, engineType, scheme, categories, costUnits, provider, attributes, providerPrivateDetails string
		var costInCents, preprovision int
		var beta, deprecated, installInsidePrivateNetwork, installOutsidePrivateNetwork, supportsMultipleInstallations, supportsSharing bool
		var created, updated time.Time

		err := rows.Scan(&planId, &serviceId, &serviceName, &name, &humanName, &description, &engineVersion, &engineType, &scheme, &categories, &costInCents, &costUnits, &attributes, &installInsidePrivateNetwork, &installOutsidePrivateNetwork, &supportsMultipleInstallations, &supportsSharing, &preprovision, &beta, &provider, &providerPrivateDetails, &deprecated)
		if err != nil {
			glog.Errorf("Scan from query failed: %s\n", err.Error())
			return nil, err
		}
		var free = falsePtr()
		if costInCents == 0 {
			free = truePtr()
		}

		var attributesJson map[string]interface{}
		if err = json.Unmarshal([]byte(attributes), &attributesJson); err != nil {
			glog.Errorf("Unable to unmarshal attributes in plans query: %s\n", err.Error())
			return nil, err
		}
		var state = "ga"
		if beta == true {
			state = "beta"
		}
		if deprecated == true {
			state = "deprecated"
		}
		plans = append(plans, ProviderPlan{
			basePlan: osb.Plan{
				ID:          planId,
				Name:        name,
				Description: description,
				Free:        free,
				Schemas: &osb.Schemas{
					ServiceInstance: &osb.ServiceInstanceSchema{
						Create: &osb.InputParametersSchema{},
					},
				},
				Metadata: map[string]interface{}{
					"addon_service": map[string]interface{}{
						"id":   serviceId,
						"name": serviceName,
					},
					"created_at":                          created,
					"description":                         description,
					"human_name":                          humanName,
					"id":                                  planId,
					"installable_inside_private_network":  installInsidePrivateNetwork,
					"installable_outside_private_network": installOutsidePrivateNetwork,
					"name":                                name,
					"key":                                 serviceName + ":" + name,
					"price": map[string]interface{}{
						"cents": costInCents,
						"unit":  costUnits,
					},
					"compliance":    []interface{}{},
					"space_default": false,
					"state":         state,
					"attributes":    attributesJson,
					"updated_at":    updated,
					"engine": map[string]string{
						"type":    engineType,
						"version": engineVersion,
					},
					"preprovision": preprovision,
				},
			},
			Provider:               GetProvidersFromString(provider),
			Scheme:                 scheme,
			providerPrivateDetails: os.ExpandEnv(providerPrivateDetails),
			ID:                     planId,
			preprovision:           preprovision,
		})
	}
	return plans, nil
}

func (b *PostgresStorage) GetServices() ([]osb.Service, error) {
	services := make([]osb.Service, 0)

	glog.V(3).Infoln("[GetServices] start")
	rows, err := b.db.Query(servicesQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var service_id, service_name, plan_human_name, plan_description, plan_categories, plan_image string
		var beta, deprecated bool
		err = rows.Scan(&service_id, &service_name, &plan_human_name, &plan_description, &plan_categories, &plan_image, &beta, &deprecated)
		if err != nil {
			return nil, err
		}

		plans, err := b.GetPlans(service_id)
		if err != nil {
			glog.Errorf("Unable to get MongoDB plans: %s\n", err.Error())
			return nil, InternalServerError()
		}

		osbPlans := make([]osb.Plan, 0)
		for _, plan := range plans {
			osbPlans = append(osbPlans, plan.basePlan)
		}
		services = append(services, osb.Service{
			Name:                service_name,
			ID:                  service_id,
			Description:         plan_description,
			Bindable:            true,
			BindingsRetrievable: true,
			PlanUpdatable:       truePtr(),
			Tags:                strings.Split(plan_categories, ","),
			Metadata: map[string]interface{}{
				"name":  plan_human_name,
				"image": plan_image,
			},
			Plans: osbPlans,
		})
	}
	return services, nil
}

func (b *PostgresStorage) GetPlanByID(planId string) (*ProviderPlan, error) {
	glog.V(4).Infof("[GetPlanById] start planId: %s", planId)

	plans, err := b.getPlans(" and plans.plan::varchar(1024) = $1::varchar(1024)", planId)
	if err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return nil, errors.New("Not found")
	}
	return &plans[0], nil
}

func (b *PostgresStorage) GetPlans(serviceId string) ([]ProviderPlan, error) {
	return b.getPlans(" and services.service::varchar(1024) = $1::varchar(1024) order by plans.name", serviceId)
}

func (b *PostgresStorage) IsUpgrading(dbId string) (bool, error) {
	var count int64
	err := b.db.QueryRow("select count(*) from tasks where ( status = 'started' or status = 'pending' ) and (action = 'change-providers' OR action = 'change-plans') and deleted = false and resource = $1", dbId).Scan(&count)
	return count > 0, err
}

func (b *PostgresStorage) IsRestoring(dbId string) (bool, error) {
	var count int64
	err := b.db.QueryRow("select count(*) from tasks where ( status = 'started' or status = 'pending' ) and action = 'restore-resource' and deleted = false and resource = $1", dbId).Scan(&count)
	return count > 0, err
}

func (b *PostgresStorage) GetUnclaimedInstance(PlanId string, InstanceId string) (*Entry, error) {
	glog.V(3).Infof("[GetUnclaimedInstance] start PlandId: %s InstanceId: %s", PlanId, InstanceId)

	tx, err := b.db.Begin()
	if err != nil {
		return nil, err
	}
	var entry Entry
	err = tx.QueryRow("select id, name, plan, claimed, status, username, password, endpoint from resources where claimed = false and status = 'available' and deleted = false and id != $1 and plan = $2 limit 1", InstanceId, PlanId).Scan(&entry.Id, &entry.Name, &entry.PlanId, &entry.Claimed, &entry.Status, &entry.Username, &entry.Password, &entry.Endpoint)
	if err != nil && err.Error() == "sql: no rows in result set" {
		tx.Rollback()
		return nil, errors.New("Cannot find resource instance")
	} else if err != nil {
		tx.Rollback()
		return nil, err
	}

	if _, err = tx.Exec("insert into resources (id, name, plan, claimed, status, username, password, endpoint) values ($1, $2, $3, true, $4, $5, $6, $7)", InstanceId, entry.Name, entry.PlanId, entry.Status, entry.Username, entry.Password, entry.Endpoint); err != nil {
		tx.Rollback()
		return nil, err
	}

	if _, err = tx.Exec("update tasks set resource = $2 where resource = $1 and deleted = false", entry.Id, InstanceId); err != nil {
		tx.Rollback()
		return nil, err
	}

	if _, err = tx.Exec("delete from resources where id = $1 and deleted = false and claimed = false", entry.Id); err != nil {
		tx.Rollback()
		return nil, err
	}

	entry.Claimed = true
	entry.Id = InstanceId

	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &entry, err
}

func (b *PostgresStorage) ReturnClaimedInstance(Id string) error {
	glog.V(4).Infof("[ReturnClaimedInstance] start Id: %s\n", Id)
	rows, err := b.db.Exec("update resources set claimed = false, id = uuid_generate_v4()::varchar(1024) where id = $1 and status = 'available' and deleted = false and claimed = true", Id)
	if err != nil {
		return err
	}
	count, err := rows.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("invalid count returned after trying to return unclaimed db " + Id)
	}
	return nil
}

func (b *PostgresStorage) AddInstance(Instance *Instance) error {
	_, err := b.db.Exec("insert into resources (id, name, plan, claimed, status, username, password, endpoint) values ($1, $2, $3, true, $4, $5, $6, $7)", Instance.Id, Instance.Name, Instance.Plan.ID, Instance.Status, Instance.Username, Instance.Password, Instance.Endpoint)
	return err
}

func (b *PostgresStorage) NukeInstance(Id string) error {
	_, err := b.db.Exec("delete from resources where id = $1", Id)
	return err
}

func (b *PostgresStorage) DeleteInstance(Instance *Instance) error {
	b.db.Exec("update tasks set deleted = true where resource = $1", Instance.Id)
	_, err := b.db.Exec("update resources set deleted = true where id = $1", Instance.Id)
	return err
}

func (b *PostgresStorage) UpdateInstance(Instance *Instance, PlanId string) error {
	_, err := b.db.Exec("update resources set plan = $1, endpoint = $2, status = $3, username = $4, password = $5, name = $6 where id = $7", PlanId, Instance.Endpoint, Instance.Status, Instance.Username, Instance.Password, Instance.Name, Instance.Id)
	return err
}

func (b *PostgresStorage) ValidateInstanceID(id string) error {
	var count int64
	glog.V(4).Infof("[ValidateInstanceID] start: %s\n", id)
	err := b.db.QueryRow("select count(*) from resources where id = $1", id).Scan(&count)
	if err != nil {
		return err
	}
	if count != 0 {
		return errors.New("The instance id is already in use (even if deleted)")
	}
	return nil
}

func (b *PostgresStorage) StartProvisioningTasks() ([]Entry, error) {
	var sqlSelectToProvisionQuery = `
        select 
            plans.plan,
            plans.preprovision - ( select count(*) from resources where resources.claimed = false and (resources.status = 'available' or resources.status = 'creating' or resources.status = 'provisioning' or resources.status = 'backing-up' or resources.status = 'starting') and resources.deleted = false and plan = plans.plan ) as needed
        from 
            plans join services on plans.service = services.service 
        where 
            plans.deprecated = false and 
            plans.deleted = false and 
            services.deleted = false and 
            services.deprecated = false
    `

	glog.V(4).Infoln("[StartPrevisionTasks] begin")
	rows, err := b.db.Query(sqlSelectToProvisionQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	glog.V(4).Infof("[StartProvisioningTasks] %-v\n", rows)

	entries := make([]Entry, 0)

	for rows.Next() {
		var planId string
		var needed int
		if err := rows.Scan(&planId, &needed); err != nil {
			return nil, err
		}
		for i := 0; i < needed; i++ {
			var entry Entry
			if err := b.db.QueryRow("insert into resources (id, name, plan, claimed, status, username, password, endpoint) values (uuid_generate_v4(), '', $1, false, 'provisioning', '', '', '') returning id", planId).Scan(&entry.Id); err != nil {
				glog.Infof("Unable to insert resource entry for preprovisioning: %s\n", err.Error())
			} else {
				entry.PlanId = planId
				entries = append(entries, entry)
			}
		}
	}

	glog.V(4).Infoln("[StartPrevisionTasks] end")

	return entries, nil
}

func (b *PostgresStorage) GetInstance(Id string) (*Entry, error) {
	var entry Entry

	glog.V(4).Infof("[GetInstance] start: %s\n", Id)
	err := b.db.QueryRow("select id, name, plan, claimed, status, username, password, endpoint, (select count(*) from tasks where tasks.resource=resources.id and tasks.status = 'started' and tasks.deleted = false) as tasks from resources where id = $1 and deleted = false", Id).Scan(&entry.Id, &entry.Name, &entry.PlanId, &entry.Claimed, &entry.Status, &entry.Username, &entry.Password, &entry.Endpoint, &entry.Tasks)

	if err != nil && err.Error() == "sql: no rows in result set" {
		return nil, errors.New("Cannot find resource instance")
	} else if err != nil {
		return nil, err
	}
	return &entry, nil
}

func (b *PostgresStorage) AddTask(Id string, action TaskAction, metadata string) (string, error) {
	var task_id string
	glog.V(4).Infof("[AddTask] start: %s\n", Id)
	return task_id, b.db.QueryRow("insert into tasks (task, resource, action, metadata) values (uuid_generate_v4(), $1, $2, $3) returning task", Id, action, metadata).Scan(&task_id)
}

func (b *PostgresStorage) UpdateTask(Id string, status *string, retries *int64, metadata *string, result *string, started *time.Time, finsihed *time.Time) error {
	glog.V(4).Infof("[UpdateTask] start: %s\n", Id)
	_, err := b.db.Exec("update tasks set status = coalesce($2, status), retries = coalesce($3, retries), metadata = coalesce($4, metadata), result = coalesce($5, result), started = coalesce($6, started), finished = coalesce($7, finished) where task = $1", Id, status, retries, metadata, result, started, finsihed)
	return err
}

func (b *PostgresStorage) WarnOnUnfinishedTasks() {
	var amount int
	err := b.db.QueryRow("select count(*) from tasks where status = 'started' and extract(hours from now() - started) > 24 and deleted = false").Scan(&amount)
	if err != nil {
		glog.Errorf("Unable to select stale tasks: %s\n", err.Error())
		return
	}
	if amount < 0 {
		glog.Errorf("WARNING: There are %d started tasks that are now over 24 hours old and have not yet finished, they may be stale.\n", amount)
	}
}

func (b *PostgresStorage) PopPendingTask() (*Task, error) {
	var task Task
	err := b.db.QueryRow(`
        update tasks set 
            status = 'started', 
            started = now() 
        where 
            task in ( select task from tasks where status = 'pending' and deleted = false order by updated asc limit 1)
        returning task, action, resource, status, retries, metadata, result, started, finished
    `).Scan(&task.Id, &task.Action, &task.ResourceId, &task.Status, &task.Retries, &task.Metadata, &task.Result, &task.Started, &task.Finished)
	if err != nil {
		return nil, err
	}
	return &task, nil
}

func redactDatabaseURL(dburl string) string {
	pstr, err := pq.ParseURL(dburl)

	if err != nil {
		return dburl
	}

	elem := strings.Split(pstr, " ")

	vars := map[string]string{}

	for _, v := range elem {
		a := strings.Split(v, "=")
		vars[a[0]] = a[1]
	}

	return fmt.Sprintf("postgres://[REDACTED]:[REDACTED]@%s:%s/%s", vars["host"], vars["port"], vars["dbname"])
}

func InitStorage(ctx context.Context, o Options) (*PostgresStorage, error) {
	// Sanity checks
	if o.DatabaseUrl == "" && os.Getenv("DATABASE_URL") != "" {
		o.DatabaseUrl = os.Getenv("DATABASE_URL")
	}
	if o.DatabaseUrl == "" {
		return nil, errors.New("unable to connect to database, none was specified in the environment via DATABASE_URL or through the -database-url cli option")
	}

	glog.Infof("DATABASE_URL=%s", redactDatabaseURL(o.DatabaseUrl))

	db, err := sql.Open("postgres", o.DatabaseUrl)
	if err != nil {
		glog.Errorf("Unable to open database: %s\n", err.Error())
		return nil, errors.New("Unable to open database: " + err.Error())
	}

	_, err = db.Exec(sqlCreateScript)
	if err != nil {
		return nil, err
	}

	go cancelOnInterrupt(ctx, db)

	return &PostgresStorage{
		db: db,
	}, nil
}
