package v1

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "github.com/odpf/optimus/api/proto/v1"
	log "github.com/odpf/optimus/core/logger"
	"github.com/odpf/optimus/core/progress"
	"github.com/odpf/optimus/job"
	"github.com/odpf/optimus/models"
	"github.com/odpf/optimus/store"
)

type ProjectRepoFactory interface {
	New() store.ProjectRepository
}
type ProtoAdapter interface {
	FromJobProto(*pb.JobSpecification) (models.JobSpec, error)
	ToJobProto(models.JobSpec) (*pb.JobSpecification, error)
	FromProjectProto(*pb.ProjectSpecification) models.ProjectSpec
	ToProjectProto(models.ProjectSpec) *pb.ProjectSpecification
}

type runtimeServiceServer struct {
	version            string
	jobSvc             models.JobService
	adapter            ProtoAdapter
	projectRepoFactory ProjectRepoFactory

	progressObserver progress.Observer

	pb.UnimplementedRuntimeServiceServer
}

func (sv *runtimeServiceServer) Version(ctx context.Context, version *pb.VersionRequest) (*pb.VersionResponse, error) {
	log.I(fmt.Printf("client with version %s requested for ping ", version.Client))
	response := &pb.VersionResponse{
		Server: sv.version,
	}
	return response, nil
}

func (sv *runtimeServiceServer) DeploySpecification(req *pb.DeploySpecificationRequest, respStream pb.RuntimeService_DeploySpecificationServer) error {
	projectRepo := sv.projectRepoFactory.New()
	projSpec, err := projectRepo.GetByName(req.GetProjectName())
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	for _, reqJob := range req.GetJobs() {
		adaptJob, err := sv.adapter.FromJobProto(reqJob)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}

		err = sv.jobSvc.Create(adaptJob, projSpec)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}

	observers := new(progress.ObserverChain)
	observers.Join(sv.progressObserver)
	observers.Join(&jobSyncObserver{
		stream: respStream,
	})

	if err := sv.jobSvc.Sync(projSpec, observers); err != nil {
		return status.Error(codes.Internal, err.Error())
	}

	return nil
}

func (sv *runtimeServiceServer) RegisterProject(ctx context.Context, req *pb.RegisterProjectRequest) (*pb.RegisterProjectResponse, error) {
	projectRepo := sv.projectRepoFactory.New()
	if err := projectRepo.Save(sv.adapter.FromProjectProto(req.GetProject())); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &pb.RegisterProjectResponse{
		Succcess: true,
		Message:  "saved successfully",
	}, nil
}

func (sv *runtimeServiceServer) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error) {
	projectRepo := sv.projectRepoFactory.New()
	projSpec, err := projectRepo.GetByName(req.GetProjectName())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	jobSpec, err := sv.jobSvc.GetByName(req.GetJobName(), projSpec)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	jobProto, err := sv.adapter.ToJobProto(jobSpec)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &pb.GetJobResponse{
		Project: sv.adapter.ToProjectProto(projSpec),
		Job:     jobProto,
	}, nil
}

func NewRuntimeServiceServer(version string, jobSvc models.JobService,
	projectRepoFactory ProjectRepoFactory, adapter ProtoAdapter,
	progressObserver progress.Observer) *runtimeServiceServer {
	return &runtimeServiceServer{
		version:            version,
		jobSvc:             jobSvc,
		adapter:            adapter,
		projectRepoFactory: projectRepoFactory,
		progressObserver:   progressObserver,
	}
}

type jobSyncObserver struct {
	stream pb.RuntimeService_DeploySpecificationServer
	log    logrus.FieldLogger
}

func (obs *jobSyncObserver) Notify(e progress.Event) {
	switch evt := e.(type) {
	case *job.EventJobUpload:
		resp := &pb.DeploySpecificationResponse{
			Succcess: true,
			JobName:  evt.Job.Name,
		}
		if evt.Err != nil {
			resp.Succcess = false
			resp.Message = evt.Err.Error()
		}

		if err := obs.stream.Send(resp); err != nil {
			obs.log.Error(errors.Wrapf(err, "failed to send deploy spec ack for: %s", evt.Job.Name))
		}
	}
}