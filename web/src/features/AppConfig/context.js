import React, { Component } from "react";

const GitOpsContext = React.createContext();

const GitOpsProvider = ({ children }) => {
  const [name, setName] = React.useState("Mia");

  return (
    <GitOpsContext.Provider value={name}>{children}</GitOpsContext.Provider>
  );
};

const GitOpsConsumer = GitOpsContext.Consumer;
//higher order component
export function withGitOpsConsumer(Component) {
  return function ConsumerWrapper(props) {
    return (
      <GitOpsConsumer>
        {/*  returning the component that was passed in , access the possible props */}
        {(value) => <Component {...props} context={value} />}
      </GitOpsConsumer>
    );
  };
}

export { GitOpsProvider, GitOpsConsumer, GitOpsContext };
