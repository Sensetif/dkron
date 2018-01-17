package dkron

import (
	"flag"
	"fmt"
	"net/http"

	"github.com/Sirupsen/logrus"
	"github.com/abronan/valkeyrie/store"
	gin "github.com/gin-gonic/gin"
)

const pretty = "pretty"

type Transport interface {
	ServeHTTP()
}

type httpApi struct {
	agent *AgentCommand
}

func NewTransport(a *AgentCommand) Transport {
	return &httpApi{
		agent: a,
	}
}

func (h *httpApi) ServeHTTP() {
	r := gin.Default()
	if flag.Lookup("test.v") != nil {
		gin.SetMode(gin.TestMode)
	} else if log.Level >= logrus.InfoLevel {
		gin.SetMode(gin.ReleaseMode)
	}

	h.apiRoutes(r)
	h.agent.dashboardRoutes(r)

	r.Use(h.metaMiddleware())
	//r.GET("/debug/vars", expvar.Handler())

	log.WithFields(logrus.Fields{
		"address": h.agent.config.HTTPAddr,
	}).Info("api: Running HTTP server")

	go r.Run(h.agent.config.HTTPAddr)
}

func (h *httpApi) apiRoutes(r *gin.Engine) {
	v1 := r.Group("/v1")
	v1.GET("/", h.indexHandler)
	v1.GET("/members", h.membersHandler)
	v1.GET("/leader", h.leaderHandler)
	v1.POST("/leave", h.leaveHandler)

	v1.POST("/jobs", h.jobCreateOrUpdateHandler)
	v1.PATCH("/jobs", h.jobCreateOrUpdateHandler)
	// Place fallback routes last
	v1.GET("/jobs", h.jobsHandler)

	jobs := v1.Group("/jobs")
	jobs.DELETE("/:job", h.jobDeleteHandler)
	jobs.POST("/:job", h.jobRunHandler)
	// Place fallback routes last
	jobs.GET("/:job", h.jobGetHandler)
	jobs.GET("/:job/executions", h.executionsHandler)
}

func (h *httpApi) metaMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Whom", h.agent.config.NodeName)
		c.Next()
	}
}

func renderJSON(c *gin.Context, status int, v interface{}) {
	if _, ok := c.GetQuery(pretty); ok {
		c.IndentedJSON(status, v)
	} else {
		c.JSON(status, v)
	}
}

func (h *httpApi) indexHandler(c *gin.Context) {
	local := h.agent.serf.LocalMember()
	stats := map[string]map[string]string{
		"agent": {
			"name":    local.Name,
			"version": h.agent.Version,
			"backend": h.agent.config.Backend,
		},
		"serf": h.agent.serf.Stats(),
		"tags": local.Tags,
	}
	renderJSON(c, http.StatusOK, stats)
}

func (h *httpApi) jobsHandler(c *gin.Context) {
	jobs, err := h.agent.store.GetJobs()
	if err != nil {
		log.WithError(err).Error("api: Unable to get jobs, store not reachable.")
		return
	}
	renderJSON(c, http.StatusOK, jobs)
}

func (h *httpApi) jobGetHandler(c *gin.Context) {
	jobName := c.Param("job")

	job, err := h.agent.store.GetJob(jobName)
	if err != nil {
		log.Error(err)
	}
	if job == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	renderJSON(c, http.StatusOK, job)
}

func (h *httpApi) jobCreateOrUpdateHandler(c *gin.Context) {
	// Init the Job object with defaults
	job := Job{
		Concurrency: ConcurrencyAllow,
	}
	c.BindJSON(&job)

	// Get if the requested job already exist
	ej, err := h.agent.store.GetJob(job.Name)
	if err != nil && err != store.ErrKeyNotFound {
		c.AbortWithError(422, err)
		return
	}

	// If it's an existing job, lock it
	if ej != nil {
		ej.Lock()
		defer ej.Unlock()
	}

	// Save the job to the store
	if err = h.agent.store.SetJob(&job); err != nil {
		c.AbortWithError(422, err)
		return
	}

	// Save the job parent
	if err = h.agent.store.SetJobDependencyTree(&job, ej); err != nil {
		c.AbortWithError(422, err)
		return
	}

	h.agent.schedulerRestartQuery(string(h.agent.store.GetLeader()))

	c.Header("Location", fmt.Sprintf("%s/%s", c.Request.RequestURI, job.Name))
	renderJSON(c, http.StatusCreated, job)
}

func (h *httpApi) jobDeleteHandler(c *gin.Context) {
	jobName := c.Param("job")

	job, err := h.agent.store.DeleteJob(jobName)
	if err != nil {
		c.AbortWithError(http.StatusNotFound, err)
		return
	}

	h.agent.schedulerRestartQuery(string(h.agent.store.GetLeader()))
	renderJSON(c, http.StatusOK, job)
}

func (h *httpApi) jobRunHandler(c *gin.Context) {
	jobName := c.Param("job")

	job, err := h.agent.store.GetJob(jobName)
	if err != nil {
		c.AbortWithError(http.StatusNotFound, err)
		return
	}

	ex := NewExecution(job.Name)
	h.agent.RunQuery(ex)

	c.Header("Location", c.Request.RequestURI)
	c.Status(http.StatusAccepted)
	renderJSON(c, http.StatusOK, job)
}

func (h *httpApi) executionsHandler(c *gin.Context) {
	jobName := c.Param("job")

	job, err := h.agent.store.GetJob(jobName)
	if err != nil {
		c.AbortWithError(http.StatusNotFound, err)
		return
	}

	executions, err := h.agent.store.GetExecutions(job.Name)
	if err != nil {
		if err == store.ErrKeyNotFound {
			renderJSON(c, http.StatusOK, &[]Execution{})
			return
		} else {
			log.Error(err)
			return
		}
	}
	renderJSON(c, http.StatusOK, executions)
}

func (h *httpApi) membersHandler(c *gin.Context) {
	renderJSON(c, http.StatusOK, h.agent.serf.Members())
}

func (h *httpApi) leaderHandler(c *gin.Context) {
	member, err := h.agent.leaderMember()
	if err == nil {
		renderJSON(c, http.StatusOK, member)
	}
}

func (h *httpApi) leaveHandler(c *gin.Context) {
	if c.Request.Method == http.MethodGet {
		log.Warn("/leave GET is deprecated and will be removed, use POST")
	}
	if err := h.agent.serf.Leave(); err != nil {
		renderJSON(c, http.StatusOK, h.agent.listServers())
	}
}
