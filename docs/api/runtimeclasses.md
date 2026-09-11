# API Reference

Packages:

- [compute.datumapis.com/v1alpha](#computedatumapiscomv1alpha)

# compute.datumapis.com/v1alpha

Resource Types:

- [RuntimeClass](#runtimeclass)




## RuntimeClass
<sup><sup>[↩ Parent](#computedatumapiscomv1alpha )</sup></sup>






RuntimeClass is an execution tier a workload can run in. It publishes the
isolation surrounding the workload, which images run unmodified, how fast
instances start, and which lifecycle operations the tier offers.

Datum owns and publishes the catalog. Customers select a class by name on a
workload and never create one. That restriction lets the machinery behind a
class change without a customer-visible API change, as long as the contract
on this object still holds.

The class is authoritative in the platform control plane and projected
read-only into project control planes, so a customer can read the contract
they select from without reaching the platform control plane.

<table>
    <thead>
        <tr>
            <th>Name</th>
            <th>Type</th>
            <th>Description</th>
            <th>Required</th>
        </tr>
    </thead>
    <tbody><tr>
      <td><b>apiVersion</b></td>
      <td>string</td>
      <td>compute.datumapis.com/v1alpha</td>
      <td>true</td>
      </tr>
      <tr>
      <td><b>kind</b></td>
      <td>string</td>
      <td>RuntimeClass</td>
      <td>true</td>
      </tr>
      <tr>
      <td><b><a href="https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.27/#objectmeta-v1-meta">metadata</a></b></td>
      <td>object</td>
      <td>Refer to the Kubernetes API documentation for the fields of the `metadata` field.</td>
      <td>true</td>
      </tr><tr>
        <td><b><a href="#runtimeclassspec">spec</a></b></td>
        <td>object</td>
        <td>
          Spec is the published contract for this execution tier.<br/>
        </td>
        <td>false</td>
      </tr><tr>
        <td><b><a href="#runtimeclassstatus">status</a></b></td>
        <td>object</td>
        <td>
          Status is what the controller implementing this class reports about it.<br/>
          <br/>
            <i>Default</i>: map[conditions:[map[lastTransitionTime:1970-01-01T00:00:00Z message:Waiting for the class controller reason:Pending status:Unknown type:Available]]]<br/>
        </td>
        <td>false</td>
      </tr></tbody>
</table>


### RuntimeClass.spec
<sup><sup>[↩ Parent](#runtimeclass)</sup></sup>



Spec is the published contract for this execution tier.

<table>
    <thead>
        <tr>
            <th>Name</th>
            <th>Type</th>
            <th>Description</th>
            <th>Required</th>
        </tr>
    </thead>
    <tbody><tr>
        <td><b><a href="#runtimeclassspeccapabilities">capabilities</a></b></td>
        <td>object</td>
        <td>
          What this class can serve, and what it cannot.<br/>
        </td>
        <td>true</td>
      </tr><tr>
        <td><b>controllerName</b></td>
        <td>string</td>
        <td>
          The controller that implements this class. A provider watches for classes
carrying its own controller name, claims them, and reports through the
Available condition whether it can honor what they declare. A class whose
controller never appears stays unclaimed, which this field makes visible.

The field says which provider realizes the class. It does not say where
the class can run. Cells advertise that separately, and placement uses
their declaration.<br/>
          <br/>
            <i>Validations</i>:<li>self == oldSelf: controllerName is immutable</li>
        </td>
        <td>true</td>
      </tr><tr>
        <td><b><a href="#runtimeclassspecisolation">isolation</a></b></td>
        <td>object</td>
        <td>
          What separates a workload in this class from other tenants' workloads.<br/>
        </td>
        <td>true</td>
      </tr><tr>
        <td><b>default</b></td>
        <td>boolean</td>
        <td>
          Whether an instance that selects no class runs in this one.

Admission stamps the default onto a workload and never resolves it at
read time. Moving the marker changes what new workloads get and leaves
running ones in the tier, cost, and startup profile they were created
with. At most one class in the catalog may set it.<br/>
        </td>
        <td>false</td>
      </tr><tr>
        <td><b>description</b></td>
        <td>string</td>
        <td>
          What this tier is for and who should choose it.<br/>
        </td>
        <td>false</td>
      </tr><tr>
        <td><b>displayName</b></td>
        <td>string</td>
        <td>
          The name to show a customer choosing a tier, for example "Unikernel fast
path".<br/>
        </td>
        <td>false</td>
      </tr><tr>
        <td><b><a href="#runtimeclassspeclifecycle">lifecycle</a></b></td>
        <td>object</td>
        <td>
          How quickly instances in this class start, and what can be done to them
once they are running.<br/>
        </td>
        <td>false</td>
      </tr></tbody>
</table>


### RuntimeClass.spec.capabilities
<sup><sup>[↩ Parent](#runtimeclassspec)</sup></sup>



What this class can serve, and what it cannot.

<table>
    <thead>
        <tr>
            <th>Name</th>
            <th>Type</th>
            <th>Description</th>
            <th>Required</th>
        </tr>
    </thead>
    <tbody><tr>
        <td><b>compatibility</b></td>
        <td>string</td>
        <td>
          What runs unmodified in this class and what does not. Customers need this
statement before committing an image to the tier.<br/>
        </td>
        <td>false</td>
      </tr><tr>
        <td><b>features</b></td>
        <td>[]enum</td>
        <td>
          The optional parts of the instance API this class serves. Anything absent
is unsupported, so a class that omits a feature rejects requests for it
rather than serving it by accident.<br/>
          <br/>
            <i>Enum</i>: sandboxRuntime, virtualMachineRuntime, configMapVolumes, secretVolumes, diskVolumes, deviceVolumeAttachments, envFrom, imagePullSecrets<br/>
        </td>
        <td>false</td>
      </tr></tbody>
</table>


### RuntimeClass.spec.isolation
<sup><sup>[↩ Parent](#runtimeclassspec)</sup></sup>



What separates a workload in this class from other tenants' workloads.

<table>
    <thead>
        <tr>
            <th>Name</th>
            <th>Type</th>
            <th>Description</th>
            <th>Required</th>
        </tr>
    </thead>
    <tbody><tr>
        <td><b>boundary</b></td>
        <td>string</td>
        <td>
          A short, stable token for the boundary, for example "unikernel" or
"virtual-machine". The values are deliberately not enumerated. The
boundaries the platform offers grow with the catalog, and fixing them in
the schema would make each new tier an API change.<br/>
        </td>
        <td>true</td>
      </tr><tr>
        <td><b>description</b></td>
        <td>string</td>
        <td>
          A description of the boundary and what it separates, suitable for a
customer to show an auditor.<br/>
        </td>
        <td>false</td>
      </tr></tbody>
</table>


### RuntimeClass.spec.lifecycle
<sup><sup>[↩ Parent](#runtimeclassspec)</sup></sup>



How quickly instances in this class start, and what can be done to them
once they are running.

<table>
    <thead>
        <tr>
            <th>Name</th>
            <th>Type</th>
            <th>Description</th>
            <th>Required</th>
        </tr>
    </thead>
    <tbody><tr>
        <td><b>description</b></td>
        <td>string</td>
        <td>
          Anything about startup or lifecycle a customer needs that the fields
above cannot express.<br/>
        </td>
        <td>false</td>
      </tr><tr>
        <td><b>operations</b></td>
        <td>[]enum</td>
        <td>
          The lifecycle operations this class offers. Declaring none is accurate
for a class whose isolation boundary does not allow them.<br/>
          <br/>
            <i>Enum</i>: Suspend, Resume, Snapshot<br/>
        </td>
        <td>false</td>
      </tr><tr>
        <td><b>typicalStartupTime</b></td>
        <td>string</td>
        <td>
          The cold start a customer should plan for, measured from instance
creation to the instance running. Startup time is the main difference
between tiers, so the class publishes it rather than leaving customers to
measure it.<br/>
        </td>
        <td>false</td>
      </tr></tbody>
</table>


### RuntimeClass.status
<sup><sup>[↩ Parent](#runtimeclass)</sup></sup>



Status is what the controller implementing this class reports about it.

<table>
    <thead>
        <tr>
            <th>Name</th>
            <th>Type</th>
            <th>Description</th>
            <th>Required</th>
        </tr>
    </thead>
    <tbody><tr>
        <td><b><a href="#runtimeclassstatusconditionsindex">conditions</a></b></td>
        <td>[]object</td>
        <td>
          <br/>
        </td>
        <td>false</td>
      </tr></tbody>
</table>


### RuntimeClass.status.conditions[index]
<sup><sup>[↩ Parent](#runtimeclassstatus)</sup></sup>



Condition contains details for one aspect of the current state of this API Resource.

<table>
    <thead>
        <tr>
            <th>Name</th>
            <th>Type</th>
            <th>Description</th>
            <th>Required</th>
        </tr>
    </thead>
    <tbody><tr>
        <td><b>lastTransitionTime</b></td>
        <td>string</td>
        <td>
          lastTransitionTime is the last time the condition transitioned from one status to another.
This should be when the underlying condition changed.  If that is not known, then using the time when the API field changed is acceptable.<br/>
          <br/>
            <i>Format</i>: date-time<br/>
        </td>
        <td>true</td>
      </tr><tr>
        <td><b>message</b></td>
        <td>string</td>
        <td>
          message is a human readable message indicating details about the transition.
This may be an empty string.<br/>
        </td>
        <td>true</td>
      </tr><tr>
        <td><b>reason</b></td>
        <td>string</td>
        <td>
          reason contains a programmatic identifier indicating the reason for the condition's last transition.
Producers of specific condition types may define expected values and meanings for this field,
and whether the values are considered a guaranteed API.
The value should be a CamelCase string.
This field may not be empty.<br/>
        </td>
        <td>true</td>
      </tr><tr>
        <td><b>status</b></td>
        <td>enum</td>
        <td>
          status of the condition, one of True, False, Unknown.<br/>
          <br/>
            <i>Enum</i>: True, False, Unknown<br/>
        </td>
        <td>true</td>
      </tr><tr>
        <td><b>type</b></td>
        <td>string</td>
        <td>
          type of condition in CamelCase or in foo.example.com/CamelCase.<br/>
        </td>
        <td>true</td>
      </tr><tr>
        <td><b>observedGeneration</b></td>
        <td>integer</td>
        <td>
          observedGeneration represents the .metadata.generation that the condition was set based upon.
For instance, if .metadata.generation is currently 12, but the .status.conditions[x].observedGeneration is 9, the condition is out of date
with respect to the current state of the instance.<br/>
          <br/>
            <i>Format</i>: int64<br/>
            <i>Minimum</i>: 0<br/>
        </td>
        <td>false</td>
      </tr></tbody>
</table>
