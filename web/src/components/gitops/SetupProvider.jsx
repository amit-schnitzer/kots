import React, { useContext } from "react";
import Select from "react-select";
import Modal from "react-modal";
import { Utilities } from "../../utilities/utilities";

import styled from "styled-components";
import DisableModal from "./modals/DisableModal";
import {
  GitOpsContext,
  withGitOpsConsumer,
} from "../../features/AppConfig/context";
import AppSelector from "../../features/AppConfig/components/AppSelector";
import { getLabel } from "../../features/AppConfig/utils";

const BITBUCKET_SERVER_DEFAULT_HTTP_PORT = "7990";
const BITBUCKET_SERVER_DEFAULT_SSH_PORT = "7999";

const SetupProvider = ({
  step,
  appsList,
  state,
  provider,
  updateHttpPort,
  updateSSHPort,
  updateSettings,
  isSingleApp,
  renderGitOpsProviderSelector,
  renderHostName,
  handleAppChange,
  selectedApp,
  finishSetup,
  getAppsList,
  getGitops,
}) => {
  const {
    owner,
    repo,
    branch,
    path,
    hostname,
    httpPort,
    sshPort,
    services,
    selectedService,
    providerError,
    finishingSetup,
  } = state;

  const [app, setApp] = React.useState({});
  const apps = appsList?.map((app) => ({
    ...app,
    value: app.name,
    label: app.name,
  }));

  React.useEffect(() => {
    //TO DO: will refactor in next PR
    const apps = appsList?.map((app) => ({
      ...app,
      value: app.name,
      label: app.name,
    }));
    if (appsList.length > 0) {
      setApp(
        apps.find((app) => {
          return app.id === selectedApp?.id;
        })
      );
    }
  }, [selectedApp, appsList]);

  const [showDisableGitopsModalPrompt, setShowDisableGitopsModalPrompt] =
    React.useState(false);
  const [disablingGitOps, setDisablingGitOps] = React.useState(false);

  const promptToDisableGitOps = () => {
    setShowDisableGitopsModalPrompt(true);
  };

  const disableGitOps = async () => {
    setDisablingGitOps(true);

    const appId = app?.id;
    let clusterId;
    if (app?.downstream) {
      clusterId = app.downstream.cluster.id;
    }

    try {
      const res = await fetch(
        `${process.env.API_ENDPOINT}/gitops/app/${appId}/cluster/${clusterId}/disable`,
        {
          headers: {
            Authorization: Utilities.getToken(),
            "Content-Type": "application/json",
          },
          method: "POST",
        }
      );
      if (!res.ok && res.status === 401) {
        Utilities.logoutUser();
        return;
      }
      if (res.ok && res.status === 204) {
        getAppsList();
        getGitops();

        setShowDisableGitopsModalPrompt(false);
      }
    } catch (err) {
      console.log(err);
    } finally {
      setDisablingGitOps(false);
    }
  };

  const downstream = app?.downstream;
  const gitops = downstream?.gitops;
  const gitopsEnabled = gitops?.enabled;
  const gitopsConnected = gitops?.isConnected;

  return (
    <div
      key={`${step.step}-active`}
      className="GitOpsDeploy--step u-textAlign--left"
    >
      <p className="step-title">{step.title}</p>
      <p className="step-sub">
        Connect a git version control system so all application updates are
        committed to a git <br />
        repository. When GitOps is enabled, you cannot deploy updates directly
        from the <br />
        admin console.
      </p>
      <div className="flex-column u-textAlign--left ">
        <div className="flex alignItems--center u-marginBottom--30">
          {isSingleApp && app ? (
            <div className="u-marginRight--5">{getLabel(app)}</div>
          ) : (
            <AppSelector
              apps={apps}
              selectedApp={selectedApp}
              handleAppChange={handleAppChange}
              isSingleApp={isSingleApp}
            />
          )}
          <div className="flex flex1 flex-column u-fontSize--small u-marginTop--20">
            {gitopsEnabled && gitopsConnected && (
              <a
                style={{ color: "blue", cursor: "pointer" }}
                disabled={disablingGitOps}
                onClick={promptToDisableGitOps}
              >
                {disablingGitOps
                  ? "Disabling GitOps"
                  : "Disable GitOps for this app"}
              </a>
            )}
          </div>
        </div>
        {renderGitOpsProviderSelector({
          owner,
          repo,
          branch,
          path,
          provider,
          hostname,
          httpPort,
          sshPort,
          providerError,
          services,
          selectedService,
        })}
      </div>
      <div>
        <DisableModal
          isOpen={showDisableGitopsModalPrompt}
          setOpen={setShowDisableGitopsModalPrompt}
          disableGitOps={disableGitOps}
          provider={provider}
        />
      </div>
    </div>
  );
};

export default withGitOpsConsumer(SetupProvider);
