within FTW;
model TwoNodeHomeThermalTwin
  "Two-state home model with separate indoor air and building mass"

  parameter Modelica.Units.SI.ThermalConductance heatLossCoefficient = 160
    "Envelope and ventilation heat loss";
  parameter Modelica.Units.SI.ThermalConductance massCoupling = 900
    "Heat transfer between indoor air and effective building mass";
  parameter Modelica.Units.SI.HeatCapacity airCapacity = 4.32e6
    "Effective air and fast contents heat capacity";
  parameter Modelica.Units.SI.HeatCapacity massCapacity = 50.4e6
    "Effective slow building mass heat capacity";
  parameter Modelica.Units.SI.Temperature initialIndoorTemperature = 293.65;
  parameter Modelica.Units.SI.Temperature initialMassTemperature = 293.15;
  parameter Real copAtReference(min = 0) = 3.4;
  parameter Real copSlopePerK = 0.05;
  parameter Modelica.Units.SI.Temperature copReferenceTemperature = 280.15;
  parameter Real minimumCOP(min = 0) = 1.5;
  parameter Real maximumCOP(min = minimumCOP) = 5.5;

  Modelica.Blocks.Interfaces.RealInput outsideTemperatureC(unit = "degC");
  Modelica.Blocks.Interfaces.RealInput heatPumpElectricPowerW(
    unit = "W",
    min = 0);
  Modelica.Blocks.Interfaces.RealInput disturbanceHeatW(unit = "W");
  Modelica.Blocks.Interfaces.RealInput nativeLoadW(unit = "W", min = 0);
  Modelica.Blocks.Interfaces.RealInput pvPowerW(unit = "W", max = 0);
  Modelica.Blocks.Interfaces.RealInput batteryPowerW(unit = "W");

  Modelica.Blocks.Interfaces.RealOutput indoorTemperatureC(unit = "degC");
  Modelica.Blocks.Interfaces.RealOutput massTemperatureC(unit = "degC");
  Modelica.Blocks.Interfaces.RealOutput heatPumpThermalPowerW(unit = "W");
  Modelica.Blocks.Interfaces.RealOutput cop;
  Modelica.Blocks.Interfaces.RealOutput gridPowerW(unit = "W");

  Modelica.Thermal.HeatTransfer.Components.HeatCapacitor indoorAir(
    C = airCapacity,
    T(start = initialIndoorTemperature, fixed = true));
  Modelica.Thermal.HeatTransfer.Components.HeatCapacitor buildingMass(
    C = massCapacity,
    T(start = initialMassTemperature, fixed = true));
  Modelica.Thermal.HeatTransfer.Components.ThermalConductor envelope(
    G = heatLossCoefficient);
  Modelica.Thermal.HeatTransfer.Components.ThermalConductor airMass(
    G = massCoupling);
  Modelica.Thermal.HeatTransfer.Sources.PrescribedTemperature outsideBoundary;
  Modelica.Thermal.HeatTransfer.Sources.PrescribedHeatFlow heatPumpHeat;
  Modelica.Thermal.HeatTransfer.Sources.PrescribedHeatFlow residualHeat;

equation
  outsideBoundary.T = outsideTemperatureC + 273.15;
  cop = min(
    maximumCOP,
    max(
      minimumCOP,
      copAtReference
        + copSlopePerK
          * (outsideBoundary.T - copReferenceTemperature)));
  heatPumpThermalPowerW = cop * heatPumpElectricPowerW;
  heatPumpHeat.Q_flow = heatPumpThermalPowerW;
  residualHeat.Q_flow = disturbanceHeatW;
  indoorTemperatureC = indoorAir.T - 273.15;
  massTemperatureC = buildingMass.T - 273.15;
  gridPowerW = nativeLoadW + heatPumpElectricPowerW + pvPowerW + batteryPowerW;

  connect(outsideBoundary.port, envelope.port_a);
  connect(envelope.port_b, indoorAir.port);
  connect(indoorAir.port, airMass.port_a);
  connect(airMass.port_b, buildingMass.port);
  connect(heatPumpHeat.port, indoorAir.port);
  connect(residualHeat.port, indoorAir.port);

  annotation (
    experiment(
      StartTime = 0,
      StopTime = 604800,
      Interval = 300,
      Tolerance = 1e-6),
    Documentation(info = "<html>
<p>This model maps directly to the optimizer's
<code>ftw-2r2c-v1</code> model. The indoor air state responds quickly while
the building mass state captures slower heat storage.</p>
</html>"));
end TwoNodeHomeThermalTwin;
