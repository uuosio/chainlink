package web

import (
	"fmt"
	"net/http"

	"chainlink/core/services"
	"chainlink/core/services/chainlink"
	"chainlink/core/store/models"
	"chainlink/core/store/orm"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// BridgeTypesController manages BridgeType requests in the node.
type BridgeTypesController struct {
	App chainlink.Application
}

// Create adds the BridgeType to the given context.
func (btc *BridgeTypesController) Create(c *gin.Context) {
	btr := &models.BridgeTypeRequest{}

	if err := c.ShouldBindJSON(btr); err != nil {
		jsonAPIError(c, http.StatusUnprocessableEntity, err)
		return
	}
	bta, bt, err := models.NewBridgeType(btr)
	if err != nil {
		jsonAPIError(c, StatusCodeForError(err), err)
		return
	}
	if err := services.ValidateBridgeType(btr, btc.App.GetStore()); err != nil {
		jsonAPIError(c, http.StatusBadRequest, err)
		return
	}
	if err := services.ValidateBridgeTypeNotExist(btr, btc.App.GetStore()); err != nil {
		jsonAPIError(c, http.StatusBadRequest, err)
		return
	}
	if err := btc.App.GetStore().CreateBridgeType(bt); err != nil {
		jsonAPIError(c, http.StatusInternalServerError, err)
		return
	}
	if errors.Cause(err) == orm.ErrorConflict {
		jsonAPIError(c, http.StatusConflict, fmt.Errorf("Bridge Type %v conflict", bt.Name))
		return
	}

	jsonAPIResponse(c, bta, "bridge")
}

// Index lists Bridges, one page at a time.
func (btc *BridgeTypesController) Index(c *gin.Context, size, page, offset int) {
	bridges, count, err := btc.App.GetStore().BridgeTypes(offset, size)
	paginatedResponse(c, "Bridges", size, page, bridges, count, err)
}

// Show returns the details of a specific Bridge.
func (btc *BridgeTypesController) Show(c *gin.Context) {
	name := c.Param("BridgeName")

	taskType, err := models.NewTaskType(name)
	if err != nil {
		jsonAPIError(c, http.StatusUnprocessableEntity, err)
		return
	}

	bt, err := btc.App.GetStore().FindBridge(taskType)
	if errors.Cause(err) == orm.ErrorNotFound {
		jsonAPIError(c, http.StatusNotFound, errors.New("bridge not found"))
		return
	}
	if err != nil {
		jsonAPIError(c, http.StatusInternalServerError, err)
		return
	}

	jsonAPIResponse(c, bt, "bridge")
}

// Update can change the restricted attributes for a bridge
func (btc *BridgeTypesController) Update(c *gin.Context) {
	name := c.Param("BridgeName")
	btr := &models.BridgeTypeRequest{}

	taskType, err := models.NewTaskType(name)
	if err != nil {
		jsonAPIError(c, http.StatusUnprocessableEntity, err)
		return
	}

	bt, err := btc.App.GetStore().FindBridge(taskType)
	if errors.Cause(err) == orm.ErrorNotFound {
		jsonAPIError(c, http.StatusNotFound, errors.New("bridge not found"))
		return
	}
	if err != nil {
		jsonAPIError(c, http.StatusInternalServerError, err)
		return
	}

	if err := c.ShouldBindJSON(btr); err != nil {
		jsonAPIError(c, http.StatusUnprocessableEntity, err)
		return
	}
	if err := services.ValidateBridgeType(btr, btc.App.GetStore()); err != nil {
		jsonAPIError(c, http.StatusBadRequest, err)
		return
	}
	if err := btc.App.GetStore().UpdateBridgeType(&bt, btr); err != nil {
		jsonAPIError(c, http.StatusInternalServerError, err)
		return
	}

	jsonAPIResponse(c, bt, "bridge")
}

// Destroy removes a specific Bridge.
func (btc *BridgeTypesController) Destroy(c *gin.Context) {
	name := c.Param("BridgeName")

	taskType, err := models.NewTaskType(name)
	if err != nil {
		jsonAPIError(c, http.StatusUnprocessableEntity, err)
		return
	}

	bt, err := btc.App.GetStore().FindBridge(taskType)
	if errors.Cause(err) == orm.ErrorNotFound {
		jsonAPIError(c, http.StatusNotFound, errors.New("bridge not found"))
		return
	}
	if err != nil {
		jsonAPIError(c, http.StatusInternalServerError, fmt.Errorf("Error searching for bridge for BTC Destroy: %+v", err))
		return
	}
	jobFounds, err := btc.App.GetStore().AnyJobWithType(name)
	if err != nil {
		jsonAPIError(c, http.StatusInternalServerError, fmt.Errorf("Error searching for associated jobs for BTC Destroy: %+v", err))
		return
	}
	if jobFounds {
		jsonAPIError(c, http.StatusConflict, fmt.Errorf("Can't remove the bridge because there are jobs associated with it: %+v", err))
		return
	}
	if err = btc.App.GetStore().DeleteBridgeType(&bt); err != nil {
		jsonAPIError(c, StatusCodeForError(err), fmt.Errorf("failed to initialise BTC Destroy: %+v", err))
		return
	}

	jsonAPIResponse(c, bt, "bridge")
}
