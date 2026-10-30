# Microservice Architecture Proposal

## Goal and scope:

The goal is to split the monolith into multiple microservices. The plan is to have the below microservices that can be deployed and managed independently.

1. `File management`
2. `User management/ Authentication`
3. `Admin API`
4. `Chat API`
5. `Notification API`

These are the seperations that I identified so far. The goal is to have clear separation and data ownwership. 

## Requirements

- Split the monolith into multiple microservices
- Added time limit on each API call with context. This is applicable to service to service communication and individual API call.
- Add logs for important events, errors and for debugging. Make sure that logs have enough details to debug and trace.
- Add traces and identification when making service to service request/communication. This will help to identify where request fails and use more memory or CPU.
- Include the time taken for each requests such as making API call. Database quering and include time taken for getting and http or database connection as well. Use proper context with timeout for the same.
- Log database wait time for establishing a connection and querying the database. 
- for service to service communication. Make sure that the calling servicing passing identification information and the receiver service is adding that into their logs.
- Add a gateway for managing routing, authentication and rate limiting
- Add a circuit breaker. If there is an outage or error (may be more that 5 errors per minute other than transient errors) in nia API, do open the circuit and after 5 minute of wait keep the circuit in half-open state and eventually in closed state. Show proper error such as NIA not available at the moment. Please try again after sometime.
- Service to service communication is impoprtant. UI can call public API's using the twt token assigned but we need a way to authenticate for service to service communication. Since we are using cloud run for running these I feel service mesh (istio) or mtls is a good option. I feel we can user worker identity and issue short lived tokens and use keyclock here.
- Create reusable function whereever possible and interfaces when there is a need for extensibility.
- Implement log roatation and changing logs at run time.


## Acceptance criteria: 

- All existing functionalities shoul work as it is.
- 90% of test coverage from unit tests and integration tests.
- Clear error messages and retry in consistent format.
- Retry for transient errors only.
- Bounted time limit and number of goroutines for tasks where it is needed
  (Identify where we can use the capabilities of goroutine)

## Implementation plan:

- Take one micro service at a time and implement one by one rather tha having everything completed at a stretch.
- Have the gateway implemented. Use the available gateway option and choose one. Initially once the first microservice is implemented let the gateway redirect all traffic coming for that route to that microservice and remaining to nordikdriveapi. We will replace it part by part.
- Have plan for service to service communication.

## Open questions:

- How do we enable service to service communication and request coming from user using separate authentication method.
- Is this a good approch to split this monolith in to micro services. Is there clear separation of data. Database tables and schema owned by one microservice should not be acessible to other without making an API call.
- Is there any scope of making grpc communication between any of these microservices pair.
- Are there any frequent communication between this microservices affecting the decision. What tradoffs do we take here?

## Verification: 

- Keep all the existing testcase and make sure that they are passing
- Dont remove any testcases to make some conditions pass.
- Strengthen the testcases to verify all edge cases.
