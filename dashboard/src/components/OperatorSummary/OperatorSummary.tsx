import Alert from "components/js/Alert";
import LoadingWrapper from "components/LoadingWrapper/LoadingWrapper";
import CapabiliyLevel, { BASIC_INSTALL } from "components/OperatorView/OperatorCapabilityLevel";
import { get } from "lodash";
import { useSelector } from "react-redux";
import { Operators } from "shared/Operators";
import { IStoreState } from "shared/types";
import "./CapabilityLevel.css";

export default function OperatorSummary() {
  const { operator, isFetching, csv } = useSelector((state: IStoreState) => state.operators);
  if (isFetching || (!operator && !csv)) {
    return <LoadingWrapper />;
  }
  let capabilityLevel = "";
  let repository = "";
  let provider = "";
  let containerImage = "";
  let createdAt = "";
  if (operator) {
    const channel = Operators.getDefaultChannel(operator);
    if (!channel || !channel.currentCSVDesc) {
      return (
        <Alert theme="danger">
          Operator {operator.metadata.name} doesn't define a valid channel. This is needed to
          extract required info.
        </Alert>
      );
    }
    const { currentCSVDesc } = channel;
    capabilityLevel = get(currentCSVDesc, "annotations.capabilities", BASIC_INSTALL);
    repository = get(currentCSVDesc, "annotations.repository", "");
    provider = get(operator, "status.provider.name", "");
    containerImage = get(currentCSVDesc, "annotations.containerImage", "");
    createdAt = get(currentCSVDesc, "annotations.createdAt", "");
  } else if (csv) {
    capabilityLevel = get(csv, "metadata.annotations.capabilities", BASIC_INSTALL);
    repository = get(csv, "metadata.annotations.repository", "");
    provider = get(csv, "spec.provider.name", "");
    containerImage = get(csv, "metadata.annotations.containerImage", "");
    createdAt = get(csv, "metadata.annotations.createdAt", "");
  }
  return (
    <div className="left-menu">
      <section className="left-menu-subsection" aria-labelledby="chartinfo-versions">
        <h5 className="left-menu-subsection-title" id="chartinfo-versions">
          Capability Level
        </h5>
        <div>
          <CapabiliyLevel level={capabilityLevel} />
        </div>
      </section>
      {repository && (
        <section className="left-menu-subsection" aria-labelledby="chartinfo-versions">
          <h5 className="left-menu-subsection-title" id="chartinfo-versions">
            Repository
          </h5>
          <div>
            <a href={repository} target="_blank" rel="noopener noreferrer">
              {repository}
            </a>
          </div>
        </section>
      )}
      {provider && (
        <section className="left-menu-subsection" aria-labelledby="chartinfo-versions">
          <h5 className="left-menu-subsection-title" id="chartinfo-versions">
            Provider
          </h5>
          <div>
            <span>{provider}</span>
          </div>
        </section>
      )}
      {containerImage && (
        <section className="left-menu-subsection" aria-labelledby="chartinfo-versions">
          <h5 className="left-menu-subsection-title" id="chartinfo-versions">
            Container Image
          </h5>
          <div>
            <span>{containerImage}</span>
          </div>
        </section>
      )}
      {createdAt && (
        <section className="left-menu-subsection" aria-labelledby="chartinfo-versions">
          <h5 className="left-menu-subsection-title" id="chartinfo-versions">
            Created At
          </h5>
          <div>
            <span>{createdAt}</span>
          </div>
        </section>
      )}
    </div>
  );
}
